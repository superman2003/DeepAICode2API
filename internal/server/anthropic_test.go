package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// anthropicChatResponse 是上游返回的标准 OpenAI chat 响应（含思维链与工具调用）。
const anthropicChatResponse = `{
  "id": "chatcmpl-1",
  "object": "chat.completion",
  "model": "glm-5.3-flash",
  "choices": [{
    "index": 0,
    "finish_reason": "tool_calls",
    "message": {
      "role": "assistant",
      "reasoning": "先看天气",
      "content": "让我查一下",
      "tool_calls": [{
        "id": "call_1",
        "type": "function",
        "function": {"name": "get_weather", "arguments": "{\"city\":\"上海\"}"}
      }]
    }
  }],
  "usage": {"prompt_tokens": 11, "completion_tokens": 22, "total_tokens": 33}
}`

// anthropicRequest 是一个覆盖 system / 图片 / 工具 / stop 的典型 Claude Code 请求。
const anthropicRequestBody = `{
  "model": "claude-sonnet-4",
  "max_tokens": 512,
  "temperature": 0.3,
  "stop_sequences": ["\n\nHuman:"],
  "system": [{"type":"text","text":"你是助手"},{"type":"text","text":"回答简短"}],
  "messages": [
    {"role":"user","content":[
      {"type":"text","text":"这是什么？"},
      {"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}
    ]}
  ],
  "tools": [{
    "name": "get_weather",
    "description": "查天气",
    "input_schema": {"type":"object","properties":{"city":{"type":"string"}}}
  }],
  "tool_choice": {"type":"any"}
}`

func TestAnthropicBlockingRoundTrip(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicChatResponse)
	})
	s, key := newTestServer(t, u)

	rec := do(t, s, http.MethodPost, "/v1/messages", key, anthropicRequestBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200：%s", rec.Code, rec.Body.String())
	}

	// --- 上游请求：Anthropic → OpenAI 的映射 ---
	up := u.last(t)
	if up.Path != "/deepai_code/chat/completions" {
		t.Fatalf("上游路径 = %q", up.Path)
	}
	if got := up.Header.Get("Api-Key"); got != "upstream-key" {
		t.Fatalf("上游 Api-Key = %q", got)
	}
	if got, _ := up.Payload["model"].(string); got == "" || got == "claude-sonnet-4" {
		t.Fatalf("model 应被改写为目录内模型，实际 %q", got)
	}
	if got, _ := up.Payload["max_tokens"].(float64); got != 512 {
		t.Fatalf("max_tokens = %v", up.Payload["max_tokens"])
	}
	if got, _ := up.Payload["temperature"].(float64); got != 0.3 {
		t.Fatalf("temperature = %v", up.Payload["temperature"])
	}
	if got, _ := up.Payload["stream"].(bool); got {
		t.Fatal("非流式请求不应带 stream=true")
	}
	if stops, _ := up.Payload["stop"].([]any); len(stops) != 1 || stops[0] != "\n\nHuman:" {
		t.Fatalf("stop_sequences 未映射: %#v", up.Payload["stop"])
	}
	if got, _ := up.Payload["tool_choice"].(string); got != "required" {
		t.Fatalf("tool_choice any 应映射为 required，实际 %#v", up.Payload["tool_choice"])
	}

	msgs, _ := up.Payload["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("消息数 = %d，期望 2（system + user）: %#v", len(msgs), msgs)
	}
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" {
		t.Fatalf("首条消息应为 system: %#v", sys)
	}
	// system 的两个文本块用 "\n" 拼接。
	if got, _ := sys["content"].(string); got != "你是助手\n回答简短" {
		t.Fatalf("system 拼接错误: %q", got)
	}
	user := msgs[1].(map[string]any)
	parts, _ := user["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("user 内容块数 = %d，期望 2（text + image）: %#v", len(parts), parts)
	}
	if parts[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("图片块应转为 image_url: %#v", parts[1])
	}
	imgURL := parts[1].(map[string]any)["image_url"].(map[string]any)["url"]
	if imgURL != "data:image/png;base64,AAA" {
		t.Fatalf("图片 URL 拼接错误: %v", imgURL)
	}
	tools, _ := up.Payload["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools 数 = %d", len(tools))
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["description"] != "查天气" {
		t.Fatalf("工具定义映射错误: %#v", fn)
	}
	if fn["parameters"] == nil {
		t.Fatal("input_schema 应映射为 parameters")
	}

	// --- 网关响应：OpenAI → Anthropic 的映射 ---
	out := decodeJSON[map[string]any](t, rec.Body.Bytes())
	if out["type"] != "message" || out["role"] != "assistant" {
		t.Fatalf("响应骨架错误: %#v", out)
	}
	if id, _ := out["id"].(string); !strings.HasPrefix(id, "msg_") {
		t.Fatalf("id 前缀错误: %q", id)
	}
	if out["stop_reason"] != "tool_use" {
		t.Fatalf("tool_calls 应映射为 stop_reason=tool_use，实际 %v", out["stop_reason"])
	}

	content, _ := out["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("内容块数 = %d，期望 3（thinking + text + tool_use）: %#v", len(content), content)
	}
	if b := content[0].(map[string]any); b["type"] != "thinking" || b["thinking"] != "先看天气" {
		t.Fatalf("思维链块错误: %#v", b)
	}
	if b := content[1].(map[string]any); b["type"] != "text" || b["text"] != "让我查一下" {
		t.Fatalf("文本块错误: %#v", b)
	}
	tool := content[2].(map[string]any)
	if tool["type"] != "tool_use" || tool["id"] != "call_1" || tool["name"] != "get_weather" {
		t.Fatalf("工具块错误: %#v", tool)
	}
	input, _ := tool["input"].(map[string]any)
	if input["city"] != "上海" {
		t.Fatalf("工具参数应被解析为对象: %#v", tool["input"])
	}

	usage, _ := out["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 11 || usage["output_tokens"].(float64) != 22 {
		t.Fatalf("用量映射错误: %#v", usage)
	}
}

func TestAnthropicStreamEventSequence(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{
			`{"choices":[{"index":0,"delta":{"reasoning":"想"}}]}`,
			`{"choices":[{"index":0,"delta":{"reasoning":"一下"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"你好"}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9","function":{"name":"f","arguments":"{\"a\""}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":9}}`,
			`[DONE]`,
		}
		for _, c := range chunks {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
			w.(http.Flusher).Flush()
		}
	})
	s, key := newTestServer(t, u)

	body := `{"model":"claude-sonnet-4","max_tokens":64,"stream":true,
	  "messages":[{"role":"user","content":"hi"}]}`
	rec := do(t, s, http.MethodPost, "/v1/messages", key, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d：%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if got, _ := u.last(t).Payload["stream"].(bool); !got {
		t.Fatal("上游应收到 stream=true")
	}

	events := sseEvents(t, rec.Body.String())
	names := eventNames(events)
	if len(names) < 5 {
		t.Fatalf("事件过少: %v", names)
	}
	if names[0] != "message_start" {
		t.Fatalf("首个事件应为 message_start，实际 %v", names)
	}
	if last := names[len(names)-1]; last != "message_stop" {
		t.Fatalf("末个事件应为 message_stop，实际 %q", last)
	}

	// 每个 content_block_start 都必须有配对的 content_block_stop，且 index 递增。
	var openIdx, starts, stops []float64
	for _, e := range events {
		m := e.JSON(t)
		switch e.Event {
		case "content_block_start":
			idx := m["index"].(float64)
			starts = append(starts, idx)
			openIdx = append(openIdx, idx)
			// 每开一个新块之前，上一个必须已关闭。
			if len(openIdx) > 1 {
				t.Fatalf("块 %v 未关闭就开了新块 %v", openIdx[:len(openIdx)-1], idx)
			}
		case "content_block_stop":
			idx := m["index"].(float64)
			stops = append(stops, idx)
			if len(openIdx) == 0 || openIdx[len(openIdx)-1] != idx {
				t.Fatalf("content_block_stop index=%v 与未关闭块 %v 不匹配", idx, openIdx)
			}
			openIdx = openIdx[:len(openIdx)-1]
		}
	}
	if len(openIdx) != 0 {
		t.Fatalf("有未关闭的内容块: %v", openIdx)
	}
	if len(starts) != 3 {
		t.Fatalf("内容块数 = %d，期望 3（thinking/text/tool）: %v", len(starts), starts)
	}
	for i, idx := range starts {
		if idx != float64(i) {
			t.Fatalf("块 index 应连续递增，实际 %v", starts)
		}
	}

	// 校验关键增量：思维链、文本、工具参数分片。
	var sawThinking, sawText, sawToolArgs bool
	for _, e := range events {
		if e.Event != "content_block_delta" {
			continue
		}
		delta, _ := e.JSON(t)["delta"].(map[string]any)
		switch delta["type"] {
		case "thinking_delta":
			sawThinking = true
		case "text_delta":
			if delta["text"] == "你好" {
				sawText = true
			}
		case "input_json_delta":
			if strings.Contains(delta["partial_json"].(string), `{"a"`) {
				sawToolArgs = true
			}
		}
	}
	if !sawThinking || !sawText || !sawToolArgs {
		t.Fatalf("增量事件缺失: thinking=%v text=%v tool=%v", sawThinking, sawText, sawToolArgs)
	}

	// message_delta 必须带 stop_reason 与用量。
	var sawDelta bool
	for _, e := range events {
		if e.Event != "message_delta" {
			continue
		}
		sawDelta = true
		m := e.JSON(t)
		d, _ := m["delta"].(map[string]any)
		if d["stop_reason"] != "tool_use" {
			t.Fatalf("stop_reason = %v，期望 tool_use", d["stop_reason"])
		}
		usage, _ := m["usage"].(map[string]any)
		if usage["input_tokens"].(float64) != 5 || usage["output_tokens"].(float64) != 9 {
			t.Fatalf("用量未透传: %#v", usage)
		}
	}
	if !sawDelta {
		t.Fatal("缺少 message_delta 事件")
	}
}

func TestAnthropicStopReasonMapping(t *testing.T) {
	cases := map[string]string{
		"tool_calls":     "tool_use",
		"function_call":  "tool_use",
		"length":         "max_tokens",
		"stop":           "end_turn",
		"content_filter": "end_turn",
		"":               "end_turn",
	}
	for in, want := range cases {
		if got := anthropicStopReason(in); got != want {
			t.Errorf("finish_reason=%q → %q，期望 %q", in, got, want)
		}
	}
}

func TestAnthropicRejectsEmptyMessages(t *testing.T) {
	s, key := newTestServer(t, nil)
	rec := do(t, s, http.MethodPost, "/v1/messages", key, `{"model":"m","max_tokens":10,"messages":[]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", rec.Code)
	}
	out := decodeJSON[map[string]any](t, rec.Body.Bytes())
	if out["type"] != "error" {
		t.Fatalf("应返回 Anthropic 风格错误: %#v", out)
	}
}

func TestAnthropicRejectsWrongMethod(t *testing.T) {
	s, key := newTestServer(t, nil)
	if rec := do(t, s, http.MethodGet, "/v1/messages", key, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 应返回 405，实际 %d", rec.Code)
	}
}

func TestAnthropicToolResultBecomesToolRole(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"content":"ok"}}]}`)
	})
	s, key := newTestServer(t, u)

	// assistant 发出 tool_use，user 回 tool_result：应展开为 assistant(tool_calls) + tool 消息。
	body := `{"model":"m","max_tokens":10,"messages":[
	  {"role":"assistant","content":[
	    {"type":"text","text":"好的"},
	    {"type":"tool_use","id":"call_1","name":"get_weather","input":{"city":"SH"}}
	  ]},
	  {"role":"user","content":[
	    {"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"晴 30℃"}]}
	  ]}
	]}`
	if rec := do(t, s, http.MethodPost, "/v1/messages", key, body); rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d：%s", rec.Code, rec.Body.String())
	}

	msgs, _ := u.last(t).Payload["messages"].([]any)
	if len(msgs) != 3 || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("应包含前导 system 和两个工具历史消息: %#v", msgs)
	}
	asst := msgs[1].(map[string]any)
	if asst["role"] != "assistant" || asst["content"] != "好的" {
		t.Fatalf("assistant 消息错误: %#v", asst)
	}
	calls, _ := asst["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls 数 = %d", len(calls))
	}
	if got := calls[0].(map[string]any)["function"].(map[string]any)["arguments"]; got != `{"city":"SH"}` {
		t.Fatalf("工具参数序列化错误: %v", got)
	}
	tool := msgs[2].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" || tool["content"] != "晴 30℃" {
		t.Fatalf("tool_result 映射错误: %#v", tool)
	}
}

func TestAnthropicToolChoiceMapping(t *testing.T) {
	cases := []struct {
		raw  string
		want any
	}{
		{`{"type":"auto"}`, "auto"},
		{`{"type":"any"}`, "required"},
		{`{"type":"none"}`, "none"},
		{`{"type":"tool","name":"f"}`, `map`},
		{`{"type":"tool"}`, nil},    // 缺 name 应忽略
		{`{"type":"unknown"}`, nil}, // 未知类型忽略
		{``, nil},                   // 空
	}
	for _, c := range cases {
		var raw []byte
		if c.raw != "" {
			raw = []byte(c.raw)
		}
		got := anthropicToolChoice(raw)
		if c.want == `map` {
			m, ok := got.(map[string]any)
			if !ok || m["type"] != "function" {
				t.Errorf("%s → %#v，期望 function 对象", c.raw, got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("%s → %#v，期望 %#v", c.raw, got, c.want)
		}
	}
}

func TestAnthropicSystemTextExtraction(t *testing.T) {
	cases := map[string]string{
		``:       `""`,
		`"纯字符串"`: "纯字符串",
		`[{"type":"text","text":"a"},{"type":"image"}]`: "a",
	}
	for raw, want := range cases {
		var got string
		if want == `""` {
			got = anthropicSystemText([]byte(raw))
			if got != "" {
				t.Errorf("%q → %q，期望空", raw, got)
			}
			continue
		}
		got = anthropicSystemText([]byte(raw))
		if got != want {
			t.Errorf("%q → %q，期望 %q", raw, got, want)
		}
	}
}

func TestAnthropicCountTokensMonotonic(t *testing.T) {
	s, key := newTestServer(t, nil)

	small := decodeJSON[map[string]any](t, do(t, s, http.MethodPost, "/v1/messages/count_tokens", key,
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`).Body.Bytes())
	big := decodeJSON[map[string]any](t, do(t, s, http.MethodPost, "/v1/messages/count_tokens", key,
		`{"model":"m","messages":[{"role":"user","content":"`+strings.Repeat("这是一段很长的文本。", 200)+`"}]}`).Body.Bytes())

	smallN, _ := small["input_tokens"].(float64)
	bigN, _ := big["input_tokens"].(float64)
	if smallN <= 0 {
		t.Fatalf("估算值应大于 0，实际 %v", smallN)
	}
	if bigN <= smallN {
		t.Fatalf("更长的输入应估出更多 token（%v vs %v）", bigN, smallN)
	}
}

func TestAnthropicUpstreamErrorMapping(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key","code":"invalid_api_key"}}`)
	})
	s, key := newTestServer(t, u)

	rec := do(t, s, http.MethodPost, "/v1/messages", key,
		`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	// 只有一个账号且鉴权失败：重试耗尽后回传上游状态码。
	if rec.Code == http.StatusOK {
		t.Fatalf("鉴权失败不应返回 200：%s", rec.Body.String())
	}
	out := decodeJSON[map[string]any](t, rec.Body.Bytes())
	if out["type"] != "error" {
		t.Fatalf("应返回 Anthropic 风格错误: %#v", out)
	}
	errBody, _ := out["error"].(map[string]any)
	if errBody["message"] == "" {
		t.Fatalf("错误信息为空: %#v", out)
	}
}
