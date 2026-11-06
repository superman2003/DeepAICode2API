package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"deepaicode2api/internal/deepai"
)

// handleResponses 是 /v1/responses 的入口。
//
// 两条路径：
//   - 目录里 api=openai-responses 的模型：原样透传到上游 /responses；
//   - 其余模型：把 Responses 请求翻译成 Chat Completions，再把结果
//     合成为 Responses 对象 / 事件流。
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST", "invalid_request_error")
		return
	}
	start := time.Now()

	raw, err := readBodyWithLimit(w, r)
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "请求体过大", "invalid_request_error")
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error(), "invalid_request_error")
		return
	}

	model := s.resolveModel(requestModelName(payload))
	payload["model"] = model.ID
	stream, _ := payload["stream"].(bool)
	metadata, _ := json.Marshal(payload["metadata"])
	affinity := prepareClientCache(r, payload, "codex", metadata, false)
	key := extractKey(r)
	promptLen := estimatePromptLen(payload)

	if model.IsResponses() {
		s.passthroughResponses(w, r, payload, model.ID, stream, affinity, key, start, promptLen)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), customToolContext{}, customToolNames(payload)))
	if previous := strOf(payload["previous_response_id"]); previous != "" {
		writeErr(w, http.StatusBadRequest, "Chat 桥接模式不保存对话：请传入完整 input 历史，不要使用 previous_response_id；原生 Responses 模型可直接使用该字段。", "unsupported_previous_response_id")
		return
	}

	chatPayload, err := responsesToChatPayload(payload, model.ID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	if stream {
		s.streamResponsesAsChat(w, r, chatPayload, model.ID, affinity, key, start, promptLen)
		return
	}
	s.blockingResponsesAsChat(w, r, chatPayload, model.ID, affinity, key, start, promptLen)
}

// passthroughResponses 透传上游 /responses。
func (s *Server) passthroughResponses(w http.ResponseWriter, r *http.Request, payload map[string]any, publicModel string, stream bool, affinity, key string, start time.Time, promptLen int) {
	ctx, cancel := contextWithTimeout(r, s.requestTimeout())
	defer cancel()

	// 同上：上游缺省 stream 时按 SSE 返回，必须显式下发。
	payload["stream"] = stream
	body, err := json.Marshal(payload)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "请求序列化失败", "invalid_request_error")
		return
	}
	resp, acct, err := s.openModel(ctx, deepai.PathResponses, body, affinity, s.requestGroup(r))
	if err != nil {
		s.record(key, acct, publicModel, "responses", stream, statusOf(err), start, promptLen, 0, Usage{}, err)
		writeUpstreamErr(w, err)
		return
	}
	defer resp.Body.Close()

	if !stream {
		rawResp, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		if err != nil {
			s.record(key, acct, publicModel, "responses", false, http.StatusBadGateway, start, promptLen, 0, Usage{}, err)
			writeErr(w, http.StatusBadGateway, "读取上游响应失败: "+err.Error(), "upstream_error")
			return
		}
		var out map[string]any
		if err := json.Unmarshal(rawResp, &out); err != nil {
			writeRawJSON(w, http.StatusOK, rawResp)
			return
		}
		rewriteResponsesModel(out, publicModel)
		usage := parseUsage(out["usage"])
		outLen := responsesTextLen(out)
		encoded, _ := json.Marshal(out)
		s.record(key, acct, publicModel, "responses", false, http.StatusOK, start, promptLen, outLen, usage, nil)
		writeRawJSON(w, http.StatusOK, encoded)
		return
	}

	sw := newSSEWriter(w)
	w.WriteHeader(http.StatusOK)
	var (
		usage  Usage
		outLen int
	)
	streamErr := readSSELines(resp.Body, func(line string) bool {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			return true
		}
		if strings.HasPrefix(trimmed, "event:") {
			_, _ = w.Write([]byte(trimmed + "\n"))
			return true
		}
		if !strings.HasPrefix(trimmed, "data:") {
			return true
		}
		data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if data == "[DONE]" {
			sw.done()
			return false
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(data), &obj); err != nil {
			_, _ = w.Write([]byte("data: " + data + "\n\n"))
			sw.flush()
			return true
		}
		rewriteResponsesModel(obj, publicModel)
		if respObj, ok := obj["response"].(map[string]any); ok {
			usage = mergeStreamUsage(usage, respObj["usage"])
		}
		outLen += responsesEventTextLen(obj)
		encoded, _ := json.Marshal(obj)
		_, _ = w.Write([]byte("data: " + string(encoded) + "\n\n"))
		sw.flush()
		return true
	})
	if streamErr != nil {
		payload := jsonEvent(map[string]any{
			"type": "error", "message": streamErr.Error(),
		})
		if payload != nil {
			sw.event("error", payload)
		}
	}
	s.record(key, acct, publicModel, "responses", true, statusOf(streamErr), start, promptLen, outLen, usage, streamErr)
}

// blockingResponsesAsChat 用 Chat Completions 合成 Responses 响应。
func (s *Server) blockingResponsesAsChat(w http.ResponseWriter, r *http.Request, chatPayload map[string]any, publicModel, affinity, key string, start time.Time, promptLen int) {
	ctx, cancel := contextWithTimeout(r, s.requestTimeout())
	defer cancel()

	out, acct, err := s.callJSON(ctx, deepai.PathChat, chatPayload, affinity, s.requestGroup(r))
	if err != nil {
		s.record(key, acct, publicModel, "responses", false, statusOf(err), start, promptLen, 0, Usage{}, err)
		writeUpstreamErr(w, err)
		return
	}
	respObj := chatToResponsesObject(out, publicModel)
	rewriteCustomToolOutput(respObj, responseCustomTools(r))
	usage := parseUsage(out["usage"])
	outLen := responsesTextLen(respObj)

	s.record(key, acct, publicModel, "responses", false, http.StatusOK, start, promptLen, outLen, usage, nil)
	writeJSON(w, http.StatusOK, respObj)
}

// streamResponsesAsChat 用 Chat Completions 流合成 Responses 事件流。
func (s *Server) streamResponsesAsChat(w http.ResponseWriter, r *http.Request, chatPayload map[string]any, publicModel, affinity, key string, start time.Time, promptLen int) {
	ctx, cancel := contextWithTimeout(r, s.requestTimeout())
	defer cancel()

	body, err := json.Marshal(chatPayload)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "请求序列化失败", "invalid_request_error")
		return
	}
	resp, acct, err := s.openModel(ctx, deepai.PathChat, body, affinity, s.requestGroup(r))
	if err != nil {
		s.record(key, acct, publicModel, "responses", true, statusOf(err), start, promptLen, 0, Usage{}, err)
		writeUpstreamErr(w, err)
		return
	}
	defer resp.Body.Close()

	sw := newSSEWriter(w)
	w.WriteHeader(http.StatusOK)
	gen := newResponsesStreamGen(sw, publicModel)
	gen.customTools = responseCustomTools(r)
	gen.created()

	var (
		usage            Usage
		outLen           int
		upstreamEventErr error
		sawDone          bool
	)
	streamErr := readSSELines(resp.Body, func(line string) bool {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			return true
		}
		data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if data == "[DONE]" || data == "" {
			if data == "[DONE]" {
				sawDone = true
			}
			return data != "[DONE]"
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return true
		}
		if problem, ok := chunk["error"].(map[string]any); ok {
			upstreamEventErr = fmt.Errorf("upstream stream error: %s", strOf(problem["message"]))
			return false
		}
		usage = mergeStreamUsage(usage, chunk["usage"])
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			return true
		}
		cm, _ := choices[0].(map[string]any)
		if cm == nil {
			return true
		}
		if delta, ok := cm["delta"].(map[string]any); ok {
			if v, ok := delta["reasoning"].(string); ok && v != "" {
				gen.reasoning(v)
				outLen += len([]rune(v))
			}
			if v, ok := delta["content"].(string); ok && v != "" {
				gen.textDelta(v)
				outLen += len([]rune(v))
			}
			if tcs, ok := delta["tool_calls"].([]any); ok {
				for _, tcv := range tcs {
					tc, ok := tcv.(map[string]any)
					if !ok {
						continue
					}
					id, _ := tc["id"].(string)
					name, args := "", ""
					if fn, ok := tc["function"].(map[string]any); ok {
						name, _ = fn["name"].(string)
						args, _ = fn["arguments"].(string)
					}
					gen.toolCall(intOf(tc["index"]), id, name, args)
				}
			}
		}
		if fr, ok := cm["finish_reason"].(string); ok && fr != "" {
			gen.finishReason = fr
		}
		return true
	})
	if upstreamEventErr != nil {
		streamErr = upstreamEventErr
	}
	if streamErr == nil && !sawDone && gen.finishReason == "" {
		streamErr = fmt.Errorf("upstream stream ended before a completion marker")
	}

	if streamErr != nil {
		gen.fail(streamErr)
	}
	gen.complete(usage)
	s.record(key, acct, publicModel, "responses", true, statusOf(streamErr), start, promptLen, outLen, usage, streamErr)
}

// --- Responses → Chat ---

// responsesToChatPayload 把 Responses 请求翻译成 Chat Completions。
func responsesToChatPayload(payload map[string]any, modelID string) (map[string]any, error) {
	out := map[string]any{
		"model":  modelID,
		"stream": boolOf(payload["stream"]),
	}
	if v, ok := payload["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := payload["top_p"]; ok {
		out["top_p"] = v
	}
	if v := intOf(payload["max_output_tokens"]); v > 0 {
		out["max_tokens"] = v
	}
	if v, ok := payload["prompt_cache_key"]; ok {
		out["prompt_cache_key"] = v
	}
	for _, field := range []string{"prompt_cache_retention", "prompt_cache_options", "parallel_tool_calls", "reasoning", "metadata"} {
		if value, ok := payload[field]; ok {
			out[field] = value
		}
	}
	if boolOf(payload["stream"]) {
		out["stream_options"] = map[string]any{"include_usage": true}
	}

	messages := make([]any, 0, 4)
	if instructions, ok := payload["instructions"].(string); ok && strings.TrimSpace(instructions) != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}
	switch input := payload["input"].(type) {
	case string:
		messages = append(messages, map[string]any{"role": "user", "content": input})
	case []any:
		for _, itemv := range input {
			item, ok := itemv.(map[string]any)
			if !ok {
				if s, ok := itemv.(string); ok {
					messages = append(messages, map[string]any{"role": "user", "content": s})
				}
				continue
			}
			switch strOf(item["type"]) {
			case "message", "":
				role := strOf(item["role"])
				if role == "" {
					role = "user"
				}
				messages = append(messages, map[string]any{
					"role": role, "content": responsesInputContent(item["content"]),
				})
			case "function_call", "custom_tool_call":
				callID := firstString(item["call_id"], item["id"])
				arguments := strOf(item["arguments"])
				if strOf(item["type"]) == "custom_tool_call" {
					raw, _ := json.Marshal(map[string]any{"input": strOf(item["input"])})
					arguments = string(raw)
				}
				call := map[string]any{
					"id": callID, "type": "function",
					"function": map[string]any{
						"name":      strOf(item["name"]),
						"arguments": arguments,
					},
				}
				merged := false
				if len(messages) > 0 {
					if previous, ok := messages[len(messages)-1].(map[string]any); ok && previous["role"] == "assistant" && previous["content"] == nil {
						if calls, ok := previous["tool_calls"].([]any); ok {
							previous["tool_calls"] = append(calls, call)
							merged = true
						}
					}
				}
				if !merged {
					messages = append(messages, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{call}})
				}
			case "function_call_output", "custom_tool_call_output":
				messages = append(messages, map[string]any{
					"role":         "tool",
					"tool_call_id": firstString(item["call_id"], item["id"]),
					"content":      responsesToolOutput(item["output"]),
				})
			case "reasoning":
				// 历史推理内容不再回喂上游。
			}
		}
	}
	if len(messages) == 0 {
		return nil, errNoMessages
	}
	out["messages"] = messages

	if tools, ok := payload["tools"].([]any); ok && len(tools) > 0 {
		converted := make([]any, 0, len(tools))
		for _, tv := range tools {
			t, ok := tv.(map[string]any)
			if !ok || (strOf(t["type"]) != "function" && strOf(t["type"]) != "custom") {
				continue
			}
			fn := map[string]any{"name": strOf(t["name"])}
			if d := strOf(t["description"]); d != "" {
				fn["description"] = d
			}
			if p, ok := t["parameters"]; ok {
				fn["parameters"] = p
			}
			if strOf(t["type"]) == "custom" {
				fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string", "description": "The exact free-form tool input."}}, "required": []any{"input"}, "additionalProperties": false}
			}
			if strict, ok := t["strict"].(bool); ok {
				fn["strict"] = strict
			}
			converted = append(converted, map[string]any{"type": "function", "function": fn})
		}
		if len(converted) > 0 {
			out["tools"] = converted
			if tc, ok := payload["tool_choice"]; ok {
				out["tool_choice"] = convertResponsesToolChoice(tc)
			}
		}
	}
	return out, nil
}

// responsesInputContent 转换 Responses 的 content 字段。
func responsesInputContent(v any) any {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		parts := make([]any, 0, len(c))
		for _, pv := range c {
			p, ok := pv.(map[string]any)
			if !ok {
				continue
			}
			switch strOf(p["type"]) {
			case "input_text", "output_text", "text":
				if t := strOf(p["text"]); t != "" {
					parts = append(parts, copyCacheControl(map[string]any{"type": "text", "text": t}, p))
				}
			case "input_image":
				if u := strOf(p["image_url"]); u != "" {
					parts = append(parts, map[string]any{
						"type": "image_url", "image_url": map[string]any{"url": u},
					})
				}
			case "refusal":
				if t := strOf(p["refusal"]); t != "" {
					parts = append(parts, map[string]any{"type": "text", "text": t})
				}
			}
		}
		if len(parts) == 0 {
			return ""
		}
		return parts
	}
	return ""
}

func responsesToolOutput(v any) string {
	switch o := v.(type) {
	case string:
		return o
	case nil:
		return ""
	default:
		raw, err := json.Marshal(o)
		if err != nil {
			return ""
		}
		return string(raw)
	}
}

func convertResponsesToolChoice(v any) any {
	switch tc := v.(type) {
	case string:
		return tc
	case map[string]any:
		if (strOf(tc["type"]) == "function" || strOf(tc["type"]) == "custom") && strOf(tc["name"]) != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": strOf(tc["name"])}}
		}
	}
	return nil
}

// --- Chat → Responses ---

// chatToResponsesObject 把 Chat Completions 响应合成为 Responses 对象。
func chatToResponsesObject(chat map[string]any, publicModel string) map[string]any {
	output := make([]any, 0, 3)
	outputText := ""
	reasoningText := ""

	choices, _ := chat["choices"].([]any)
	if len(choices) > 0 {
		if cm, ok := choices[0].(map[string]any); ok {
			if msg, ok := cm["message"].(map[string]any); ok {
				if r := strOf(msg["reasoning"]); r != "" {
					reasoningText = r
					output = append(output, map[string]any{
						"type": "reasoning", "id": "rs_" + deepai.RandomHex(12),
						"summary": []any{map[string]any{"type": "summary_text", "text": r}},
					})
				}
				if t := strOf(msg["content"]); t != "" {
					outputText = t
					output = append(output, map[string]any{
						"type": "message", "id": "msg_" + deepai.RandomHex(12),
						"status": "completed", "role": "assistant",
						"content": []any{map[string]any{"type": "output_text", "text": t, "annotations": []any{}}},
					})
				}
				if tcs, ok := msg["tool_calls"].([]any); ok {
					for _, tcv := range tcs {
						tc, ok := tcv.(map[string]any)
						if !ok {
							continue
						}
						callID := strOf(tc["id"])
						name, args := "", ""
						if fn, ok := tc["function"].(map[string]any); ok {
							name = strOf(fn["name"])
							args = strOf(fn["arguments"])
						}
						output = append(output, map[string]any{
							"type": "function_call", "id": "fc_" + deepai.RandomHex(12),
							"call_id": callID, "name": name, "arguments": args,
							"status": "completed",
						})
					}
				}
			}
		}
	}

	usage := parseUsage(chat["usage"])
	resp := map[string]any{
		"id":                  "resp_" + deepai.RandomHex(12),
		"object":              "response",
		"created_at":          time.Now().Unix(),
		"status":              "completed",
		"model":               publicModel,
		"output":              output,
		"output_text":         outputText,
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
		"tools":               []any{},
		"usage":               responsesUsage(usage),
		"error":               nil, "incomplete_details": nil,
		"store": false, "previous_response_id": nil, "metadata": map[string]any{},
	}
	if len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if reason := incompleteReason(strOf(choice["finish_reason"])); reason != "" {
				resp["status"] = "incomplete"
				resp["incomplete_details"] = map[string]any{"reason": reason}
			}
		}
	}
	if reasoningText != "" {
		resp["reasoning"] = map[string]any{"effort": nil, "summary": "auto"}
	}
	return resp
}

// --- Responses 流事件生成器 ---

type responsesStreamGen struct {
	sw            *sseWriter
	model         string
	respID        string
	createdAt     int64
	outputIndex   int
	textItemID    string
	textBuf       strings.Builder
	textItemBuf   strings.Builder
	textOpen      bool
	reasoningOpen bool
	reasoningID   string
	reasoningBuf  strings.Builder
	toolItems     map[int]*responsesToolState
	finishReason  string
	output        []any
	sequence      int
	streamFailure error
	customTools   map[string]bool
}

type responsesToolState struct {
	itemID   string
	callID   string
	name     string
	args     strings.Builder
	index    int
	finished bool
	custom   bool
}

func newResponsesStreamGen(sw *sseWriter, model string) *responsesStreamGen {
	return &responsesStreamGen{
		sw:        sw,
		model:     model,
		respID:    "resp_" + deepai.RandomHex(12),
		createdAt: time.Now().Unix(),
		toolItems: map[int]*responsesToolState{},
		output:    []any{},
	}
}

func (g *responsesStreamGen) emit(event string, v map[string]any) {
	v["type"] = event
	v["sequence_number"] = g.sequence
	g.sequence++
	if raw := jsonEvent(v); raw != nil {
		g.sw.event(event, raw)
	}
}

func (g *responsesStreamGen) response(status string, usage Usage) map[string]any {
	resp := map[string]any{
		"id": g.respID, "object": "response", "created_at": g.createdAt,
		"status": status, "model": g.model,
		"output":              g.output,
		"output_text":         g.textBuf.String(),
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
		"tools":               []any{},
		"error":               nil, "incomplete_details": nil,
	}
	resp["usage"] = nil
	resp["store"] = false
	resp["previous_response_id"] = nil
	resp["metadata"] = map[string]any{}
	if usage.TotalTokens > 0 || usage.CacheReadTokens > 0 || usage.CacheCreationTokens > 0 {
		resp["usage"] = responsesUsage(usage)
	}
	if status == "failed" && g.streamFailure != nil {
		resp["error"] = map[string]any{"code": "upstream_stream_error", "message": g.streamFailure.Error()}
	}
	if status == "incomplete" {
		resp["incomplete_details"] = map[string]any{"reason": incompleteReason(g.finishReason)}
	}
	return resp
}

func (g *responsesStreamGen) created() {
	g.emit("response.created", map[string]any{
		"response": g.response("in_progress", Usage{}),
	})
	g.emit("response.in_progress", map[string]any{"response": g.response("in_progress", Usage{})})
}

func (g *responsesStreamGen) reasoning(delta string) {
	if !g.reasoningOpen {
		g.closeText()
		g.reasoningOpen = true
		g.reasoningBuf.Reset()
		g.reasoningID = "rs_" + deepai.RandomHex(12)
		g.emit("response.output_item.added", map[string]any{
			"output_index": g.outputIndex,
			"item": map[string]any{
				"id": g.reasoningID, "type": "reasoning", "status": "in_progress",
				"summary": []any{},
			},
		})
		g.emit("response.reasoning_summary_part.added", map[string]any{
			"item_id": g.reasoningID, "output_index": g.outputIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		})
	}
	g.reasoningBuf.WriteString(delta)
	g.emit("response.reasoning_summary_text.delta", map[string]any{
		"item_id": g.reasoningID, "output_index": g.outputIndex, "summary_index": 0,
		"delta": delta,
	})
}

func (g *responsesStreamGen) textDelta(delta string) {
	if !g.textOpen {
		g.closeReasoning()
		g.textOpen = true
		g.textItemBuf.Reset()
		g.textItemID = "msg_" + deepai.RandomHex(12)
		g.emit("response.output_item.added", map[string]any{
			"output_index": g.outputIndex,
			"item": map[string]any{
				"id": g.textItemID, "type": "message", "status": "in_progress",
				"role": "assistant", "content": []any{},
			},
		})
		g.emit("response.content_part.added", map[string]any{
			"item_id": g.textItemID, "output_index": g.outputIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	}
	g.textBuf.WriteString(delta)
	g.textItemBuf.WriteString(delta)
	g.emit("response.output_text.delta", map[string]any{
		"item_id": g.textItemID, "output_index": g.outputIndex, "content_index": 0,
		"delta": delta,
	})
}

func (g *responsesStreamGen) toolCall(idx int, id, name, args string) {
	g.closeText()
	state, ok := g.toolItems[idx]
	if !ok {
		state = &responsesToolState{
			itemID: "fc_" + deepai.RandomHex(12),
			callID: firstString(id, "call_"+deepai.RandomHex(8)),
			name:   name,
			index:  g.outputIndex,
			custom: g.customTools[name],
		}
		g.toolItems[idx] = state
		g.outputIndex++
		item := map[string]any{"id": state.itemID, "type": "function_call", "status": "in_progress", "call_id": state.callID, "name": state.name, "arguments": ""}
		if state.custom {
			item["type"] = "custom_tool_call"
			delete(item, "arguments")
			item["input"] = ""
		}
		g.emit("response.output_item.added", map[string]any{
			"output_index": state.index,
			"item":         item,
		})
	}
	if name != "" && state.name == "" {
		state.name = name
	}
	if args != "" {
		state.args.WriteString(args)
		if !state.custom {
			g.emit("response.function_call_arguments.delta", map[string]any{
				"item_id": state.itemID, "output_index": state.index, "delta": args,
			})
		}
	}
}

func (g *responsesStreamGen) closeReasoning() {
	if !g.reasoningOpen {
		return
	}
	g.reasoningOpen = false
	text := g.reasoningBuf.String()
	item := map[string]any{
		"id": g.reasoningID, "type": "reasoning", "status": "completed",
		"summary": []any{map[string]any{"type": "summary_text", "text": text}},
	}
	g.setOutput(g.outputIndex, item)
	g.emit("response.reasoning_summary_text.done", map[string]any{
		"item_id": g.reasoningID, "output_index": g.outputIndex, "summary_index": 0, "text": text,
	})
	g.emit("response.reasoning_summary_part.done", map[string]any{"item_id": g.reasoningID, "output_index": g.outputIndex, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": text}})
	g.emit("response.output_item.done", map[string]any{
		"output_index": g.outputIndex, "item": item,
	})
	g.outputIndex++
}

func (g *responsesStreamGen) closeText() {
	if !g.textOpen {
		g.closeReasoning()
		return
	}
	g.textOpen = false
	text := g.textItemBuf.String()
	item := map[string]any{
		"id": g.textItemID, "type": "message", "status": "completed",
		"role":    "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
	}
	g.setOutput(g.outputIndex, item)
	g.emit("response.output_text.done", map[string]any{
		"item_id": g.textItemID, "output_index": g.outputIndex, "content_index": 0, "text": text,
	})
	g.emit("response.content_part.done", map[string]any{
		"item_id": g.textItemID, "output_index": g.outputIndex, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
	})
	g.emit("response.output_item.done", map[string]any{
		"output_index": g.outputIndex, "item": item,
	})
	g.outputIndex++
}

func (g *responsesStreamGen) closeTools() {
	states := make([]*responsesToolState, 0, len(g.toolItems))
	for _, state := range g.toolItems {
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].index < states[j].index })
	for _, state := range states {
		if state.finished {
			continue
		}
		state.finished = true
		args := state.args.String()
		item := map[string]any{
			"id": state.itemID, "type": "function_call", "status": "completed",
			"call_id": state.callID, "name": state.name, "arguments": args,
		}
		if state.custom {
			input := customToolInput(args)
			item["type"] = "custom_tool_call"
			delete(item, "arguments")
			item["input"] = input
			g.emit("response.custom_tool_call_input.delta", map[string]any{"item_id": state.itemID, "output_index": state.index, "delta": input})
			g.emit("response.custom_tool_call_input.done", map[string]any{"item_id": state.itemID, "output_index": state.index, "input": input})
		} else {
			g.emit("response.function_call_arguments.done", map[string]any{
				"item_id": state.itemID, "output_index": state.index, "arguments": args,
			})
		}
		g.setOutput(state.index, item)
		g.emit("response.output_item.done", map[string]any{
			"output_index": state.index, "item": item,
		})
	}
}

func (g *responsesStreamGen) setOutput(index int, item map[string]any) {
	for len(g.output) <= index {
		g.output = append(g.output, nil)
	}
	g.output[index] = item
}

func (g *responsesStreamGen) fail(err error) {
	g.streamFailure = err
	g.emit("error", map[string]any{"code": "upstream_stream_error", "message": err.Error(), "param": nil})
}

func (g *responsesStreamGen) complete(usage Usage) {
	g.closeText()
	g.closeReasoning()
	g.closeTools()
	status := "completed"
	if g.streamFailure != nil {
		status = "failed"
	} else if incompleteReason(g.finishReason) != "" {
		status = "incomplete"
	}
	g.emit("response."+status, map[string]any{
		"response": g.response(status, usage),
	})
}

func incompleteReason(finish string) string {
	if finish == "length" {
		return "max_output_tokens"
	}
	if finish == "content_filter" {
		return "content_filter"
	}
	return ""
}

// --- 工具函数 ---

var errNoMessages = &clientError{Message: "input 不能为空"}

type clientError struct{ Message string }

func (e *clientError) Error() string { return e.Message }

func boolOf(v any) bool {
	b, _ := v.(bool)
	return b
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}

// rewriteResponsesModel 把 Responses 对象 / 事件里的 model 改为对外模型名。
func rewriteResponsesModel(obj map[string]any, publicModel string) {
	if publicModel == "" {
		return
	}
	if _, ok := obj["model"]; ok {
		obj["model"] = publicModel
	}
	if resp, ok := obj["response"].(map[string]any); ok {
		rewriteResponsesModel(resp, publicModel)
	}
}

// responsesTextLen 统计 Responses 对象中的输出文本长度。
func responsesTextLen(obj map[string]any) int {
	total := 0
	if s, ok := obj["output_text"].(string); ok {
		total += len([]rune(s))
	}
	output, _ := obj["output"].([]any)
	for _, itemv := range output {
		item, ok := itemv.(map[string]any)
		if !ok {
			continue
		}
		switch strOf(item["type"]) {
		case "message":
			content, _ := item["content"].([]any)
			for _, cv := range content {
				c, ok := cv.(map[string]any)
				if ok {
					total += lenOfString(c["text"])
				}
			}
		case "reasoning":
			summary, _ := item["summary"].([]any)
			for _, sv := range summary {
				sm, ok := sv.(map[string]any)
				if ok {
					total += lenOfString(sm["text"])
				}
			}
		}
	}
	return total
}

// responsesEventTextLen 统计单个流事件的文本增量长度。
func responsesEventTextLen(obj map[string]any) int {
	if strOf(obj["type"]) == "response.output_text.delta" {
		return len([]rune(strOf(obj["delta"])))
	}
	if strOf(obj["type"]) == "response.reasoning_summary_text.delta" {
		return len([]rune(strOf(obj["delta"])))
	}
	return 0
}
