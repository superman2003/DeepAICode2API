package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestResponsesNonStreamMapping(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
		  "id":"chatcmpl-2",
		  "choices":[{"index":0,"finish_reason":"tool_calls","message":{
		    "role":"assistant",
		    "reasoning":"需要调用工具",
		    "content":"稍等",
		    "tool_calls":[{"id":"call_7","type":"function",
		      "function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]
		  }}],
		  "usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}
		}`)
	})
	s, key := newTestServer(t, u)

	body := `{
	  "model":"gpt-4o",
	  "instructions":"你是助手",
	  "max_output_tokens":128,
	  "input":[
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"帮我查"}]},
	    {"type":"function_call","call_id":"call_0","name":"lookup","arguments":"{\"q\":\"a\"}"},
	    {"type":"function_call_output","call_id":"call_0","output":"结果A"}
	  ],
	  "tools":[{"type":"function","name":"lookup","description":"查询",
	            "parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]
	}`
	rec := do(t, s, http.MethodPost, "/v1/responses", key, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d：%s", rec.Code, rec.Body.String())
	}

	// --- 上游请求映射 ---
	up := u.last(t)
	if up.Path != "/deepai_code/chat/completions" {
		t.Fatalf("非 responses 模型应改走 chat 端点，实际 %q", up.Path)
	}
	if got, _ := up.Payload["max_tokens"].(float64); got != 128 {
		t.Fatalf("max_output_tokens 应映射为 max_tokens，实际 %#v", up.Payload["max_tokens"])
	}
	msgs, _ := up.Payload["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("消息数 = %d，期望 4（system + user + assistant + tool）: %#v", len(msgs), msgs)
	}
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("instructions 应成为 system 消息: %#v", msgs[0])
	}
	if msgs[0].(map[string]any)["content"] != "你是助手" {
		t.Fatalf("instructions 内容错误: %#v", msgs[0])
	}
	if msgs[1].(map[string]any)["role"] != "user" {
		t.Fatalf("第二条应为 user: %#v", msgs[1])
	}
	asst := msgs[2].(map[string]any)
	calls, _ := asst["tool_calls"].([]any)
	if len(calls) != 1 || calls[0].(map[string]any)["id"] != "call_0" {
		t.Fatalf("function_call 应转为 assistant.tool_calls: %#v", asst)
	}
	tool := msgs[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_0" || tool["content"] != "结果A" {
		t.Fatalf("function_call_output 应转为 tool 消息: %#v", tool)
	}
	tools, _ := up.Payload["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["function"].(map[string]any)["name"] != "lookup" {
		t.Fatalf("工具映射错误: %#v", up.Payload["tools"])
	}

	// --- 网关响应：合成 Responses 对象 ---
	out := decodeJSON[map[string]any](t, rec.Body.Bytes())
	if out["object"] != "response" || out["status"] != "completed" {
		t.Fatalf("响应骨架错误: %#v", out)
	}
	if id, _ := out["id"].(string); !strings.HasPrefix(id, "resp_") {
		t.Fatalf("id 前缀错误: %q", id)
	}
	if out["output_text"] != "稍等" {
		t.Fatalf("output_text = %v", out["output_text"])
	}
	items, _ := out["output"].([]any)
	if len(items) != 3 {
		t.Fatalf("output 项数 = %d，期望 3（reasoning/message/function_call）: %#v", len(items), items)
	}
	if items[0].(map[string]any)["type"] != "reasoning" {
		t.Fatalf("首项应为 reasoning: %#v", items[0])
	}
	msgItem := items[1].(map[string]any)
	if msgItem["type"] != "message" {
		t.Fatalf("第二项应为 message: %#v", msgItem)
	}
	parts, _ := msgItem["content"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["type"] != "output_text" {
		t.Fatalf("message.content 结构错误: %#v", msgItem["content"])
	}
	fc := items[2].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_7" || fc["name"] != "lookup" {
		t.Fatalf("function_call 项错误: %#v", fc)
	}
	if fc["arguments"] != `{"q":"x"}` {
		t.Fatalf("arguments 应原样透传: %v", fc["arguments"])
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 3 || usage["output_tokens"].(float64) != 4 {
		t.Fatalf("用量映射错误: %#v", usage)
	}
	if out["reasoning"] == nil {
		t.Fatal("有思维链时应输出 reasoning 汇总")
	}
}

func TestResponsesStringInput(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"content":"ok"}}]}`)
	})
	s, key := newTestServer(t, u)

	rec := do(t, s, http.MethodPost, "/v1/responses", key,
		`{"model":"gpt-4o","input":"直接给字符串"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d：%s", rec.Code, rec.Body.String())
	}
	msgs, _ := u.last(t).Payload["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("应包含前导 system 和字符串 input: %#v", msgs)
	}
	if msgs[1].(map[string]any)["content"] != "直接给字符串" {
		t.Fatalf("字符串 input 未映射: %#v", msgs[1])
	}
}

func TestResponsesRejectsEmptyInput(t *testing.T) {
	s, key := newTestServer(t, nil)
	rec := do(t, s, http.MethodPost, "/v1/responses", key, `{"model":"gpt-4o"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400：%s", rec.Code, rec.Body.String())
	}
}

func TestResponsesStreamEventSequence(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range []string{
			`{"choices":[{"index":0,"delta":{"reasoning":"想"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"你好"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"世界"}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
			`[DONE]`,
		} {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
			w.(http.Flusher).Flush()
		}
	})
	s, key := newTestServer(t, u)

	rec := do(t, s, http.MethodPost, "/v1/responses", key,
		`{"model":"gpt-4o","stream":true,"input":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d：%s", rec.Code, rec.Body.String())
	}

	events := sseEvents(t, rec.Body.String())
	names := eventNames(events)
	if len(names) == 0 {
		t.Fatal("没有收到任何事件")
	}
	if names[0] != "response.created" {
		t.Fatalf("首事件 = %q，期望 response.created", names[0])
	}
	if last := names[len(names)-1]; last != "response.completed" {
		t.Fatalf("末事件 = %q，期望 response.completed（全部：%v）", last, names)
	}

	// 必需的事件类型齐全。
	for _, want := range []string{
		"response.output_item.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.completed",
	} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("缺少事件 %q：%v", want, names)
		}
	}

	// 文本增量拼起来应等于完整文本。
	var text strings.Builder
	for _, e := range events {
		if e.Event == "response.output_text.delta" {
			if d, ok := e.JSON(t)["delta"].(string); ok {
				text.WriteString(d)
			}
		}
	}
	if text.String() != "你好世界" {
		t.Fatalf("文本增量拼接 = %q", text.String())
	}

	// response.completed 必须带完整 response 对象。
	var completed map[string]any
	for _, e := range events {
		if e.Event == "response.completed" {
			completed, _ = e.JSON(t)["response"].(map[string]any)
		}
	}
	if completed == nil {
		t.Fatal("response.completed 缺少 response 字段")
	}
	if completed["status"] != "completed" {
		t.Fatalf("status = %v", completed["status"])
	}
	if completed["output_text"] != "你好世界" {
		t.Fatalf("output_text = %v", completed["output_text"])
	}
}

func TestResponsesFunctionCallStream(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range []string{
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{\"a\":"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`[DONE]`,
		} {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
			w.(http.Flusher).Flush()
		}
	})
	s, key := newTestServer(t, u)

	rec := do(t, s, http.MethodPost, "/v1/responses", key,
		`{"model":"gpt-4o","stream":true,"input":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d：%s", rec.Code, rec.Body.String())
	}

	events := sseEvents(t, rec.Body.String())
	var sawArgsDelta bool
	var completed map[string]any
	for _, e := range events {
		switch e.Event {
		case "response.function_call_arguments.delta":
			if strings.Contains(e.JSON(t)["delta"].(string), `{"a":`) {
				sawArgsDelta = true
			}
		case "response.completed":
			completed, _ = e.JSON(t)["response"].(map[string]any)
		}
	}
	if !sawArgsDelta {
		t.Fatalf("缺少工具参数增量事件：%v", eventNames(events))
	}
	if completed == nil {
		t.Fatal("缺少 response.completed")
	}
	items, _ := completed["output"].([]any)
	if len(items) != 1 {
		t.Fatalf("output 项数 = %d: %#v", len(items), items)
	}
	fc := items[0].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_1" {
		t.Fatalf("function_call 项错误: %#v", fc)
	}
	if fc["arguments"] != `{"a":1}` {
		t.Fatalf("参数分片未拼接: %v", fc["arguments"])
	}
}
