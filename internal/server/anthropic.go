package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"deepaicode2api/internal/deepai"
)

// --- Anthropic 请求模型 ---

type anthropicRequest struct {
	Model          string             `json:"model"`
	MaxTokens      int                `json:"max_tokens"`
	System         json.RawMessage    `json:"system"`
	Messages       []anthropicMessage `json:"messages"`
	Stream         bool               `json:"stream"`
	Temperature    *float64           `json:"temperature"`
	TopP           *float64           `json:"top_p"`
	TopK           *int               `json:"top_k"`
	StopSequences  []string           `json:"stop_sequences"`
	Tools          []anthropicTool    `json:"tools"`
	ToolChoice     json.RawMessage    `json:"tool_choice"`
	Thinking       json.RawMessage    `json:"thinking"`
	Metadata       json.RawMessage    `json:"metadata"`
	CacheControl   json.RawMessage    `json:"cache_control"`
	PromptCacheKey string             `json:"prompt_cache_key"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl json.RawMessage `json:"cache_control"`
}

// handleAnthropicMessages 是 POST /v1/messages 的入口。
//
// 把 Anthropic Messages 请求转换为 OpenAI Chat Completions，调用上游后再
// 映射回 Anthropic 的 message / SSE 事件模型（含 tool_use、thinking）。
func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "仅支持 POST")
		return
	}
	start := time.Now()

	raw, err := readBodyWithLimit(w, r)
	if err != nil {
		writeAnthropicErr(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "请求体过大")
		return
	}
	var req anthropicRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "请求体解析失败: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "messages 不能为空")
		return
	}

	model := s.resolveModel(req.Model)
	payload, err := anthropicToChatPayload(&req, model.ID)
	if err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	affinity := prepareClientCache(r, payload, "claude", req.Metadata, anthropicHasCacheControl(&req))
	key := extractKey(r)
	promptLen := estimatePromptLen(payload)

	if req.Stream {
		s.streamAnthropic(w, r, payload, model.ID, key, affinity, start, promptLen)
		return
	}
	s.blockingAnthropic(w, r, payload, model.ID, key, affinity, start, promptLen)
}

// blockingAnthropic 收集完整回复后按 Anthropic message 结构返回。
func (s *Server) blockingAnthropic(w http.ResponseWriter, r *http.Request, payload map[string]any, publicModel, key, affinity string, start time.Time, promptLen int) {
	ctx, cancel := contextWithTimeout(r, s.requestTimeout())
	defer cancel()

	out, acct, err := s.callJSON(ctx, deepai.PathChat, payload, affinity, s.requestGroup(r))
	if err != nil {
		s.record(key, acct, publicModel, "messages", false, statusOf(err), start, promptLen, 0, Usage{}, err)
		writeAnthropicUpstreamErr(w, err)
		return
	}
	usage := parseUsage(out["usage"])
	msg := chatToAnthropicMessage(out, publicModel)
	outLen := anthropicTextLen(msg)

	s.record(key, acct, publicModel, "messages", false, http.StatusOK, start, promptLen, outLen, usage, nil)
	writeJSON(w, http.StatusOK, msg)
}

// streamAnthropic 以 Anthropic SSE 事件模型流式返回。
func (s *Server) streamAnthropic(w http.ResponseWriter, r *http.Request, payload map[string]any, publicModel, key, affinity string, start time.Time, promptLen int) {
	ctx, cancel := contextWithTimeout(r, s.requestTimeout())
	defer cancel()

	body, err := json.Marshal(payload)
	if err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "请求序列化失败")
		return
	}
	resp, acct, err := s.openModel(ctx, deepai.PathChat, body, affinity, s.requestGroup(r))
	if err != nil {
		s.record(key, acct, publicModel, "messages", true, statusOf(err), start, promptLen, 0, Usage{}, err)
		writeAnthropicUpstreamErr(w, err)
		return
	}
	defer resp.Body.Close()

	sw := newSSEWriter(w)
	w.WriteHeader(http.StatusOK)

	msgID := "msg_" + deepai.RandomHex(12)
	send := func(event string, v any) {
		if raw := jsonEvent(v); raw != nil {
			sw.event(event, raw)
		}
	}

	send("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant", "model": publicModel,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": anthropicUsage(Usage{}),
		},
	})

	block := newAnthropicBlockWriter(send)
	var (
		usage      Usage
		outLen     int
		stopReason = "end_turn"
		streamErr  error
	)

	streamErr = readSSELines(resp.Body, func(line string) bool {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			return true
		}
		data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if data == "[DONE]" || data == "" {
			return data != "[DONE]"
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return true
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
		if fr, ok := cm["finish_reason"].(string); ok && fr != "" {
			stopReason = anthropicStopReason(fr)
		}
		delta, _ := cm["delta"].(map[string]any)
		if delta == nil {
			return true
		}

		if v, ok := delta["reasoning"].(string); ok && v != "" {
			block.thinking(v)
			outLen += len([]rune(v))
		}
		if v, ok := delta["content"].(string); ok && v != "" {
			block.text(v)
			outLen += len([]rune(v))
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, tcv := range tcs {
				tc, ok := tcv.(map[string]any)
				if !ok {
					continue
				}
				idx := intOf(tc["index"])
				id, _ := tc["id"].(string)
				name := ""
				args := ""
				if fn, ok := tc["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
					args, _ = fn["arguments"].(string)
				}
				block.toolCall(idx, id, name, args)
			}
		}
		return true
	})

	block.close()
	if streamErr != nil {
		send("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": streamErr.Error()},
		})
	}
	outputTokens := usage.CompletionTokens
	if outputTokens == 0 {
		outputTokens = estTokens(outLen)
	}
	usage.CompletionTokens = outputTokens
	send("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": anthropicUsage(usage),
	})
	send("message_stop", map[string]any{"type": "message_stop"})

	s.record(key, acct, publicModel, "messages", true, statusOf(streamErr), start, promptLen, outLen, usage, streamErr)
}

// handleAnthropicCountTokens 返回粗略的输入 token 估算。
//
// Anthropic 客户端（如 Claude Code）会在发送消息前调用它做预算控制；
// 上游不提供该能力，这里用字符数近似，只需单调合理即可。
func (s *Server) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicErr(w, http.StatusMethodNotAllowed, "invalid_request_error", "仅支持 POST")
		return
	}
	raw, err := readBodyWithLimit(w, r)
	if err != nil {
		writeAnthropicErr(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "请求体过大")
		return
	}
	var req anthropicRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "请求体解析失败")
		return
	}
	payload, err := anthropicToChatPayload(&req, s.resolveModel(req.Model).ID)
	if err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	rawPayload, _ := json.Marshal(payload)
	writeJSON(w, http.StatusOK, map[string]any{
		"input_tokens": estTokens(len(rawPayload)),
	})
}

// --- Anthropic → OpenAI ---

// anthropicToChatPayload 把 Anthropic 请求转换为 OpenAI Chat Completions。
func anthropicToChatPayload(req *anthropicRequest, modelID string) (map[string]any, error) {
	payload := map[string]any{
		"model":  modelID,
		"stream": req.Stream,
	}
	if req.MaxTokens > 0 {
		payload["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		payload["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		payload["stop"] = req.StopSequences
	}
	if req.Stream {
		payload["stream_options"] = map[string]any{"include_usage": true}
	}
	if req.PromptCacheKey != "" {
		payload["prompt_cache_key"] = req.PromptCacheKey
	}
	if control := decodeCacheControl(req.CacheControl); control != nil {
		payload["cache_control"] = control
	}

	messages := make([]any, 0, len(req.Messages)+1)
	if sys := anthropicSystemContent(req.System); sys != nil {
		messages = append(messages, map[string]any{"role": "system", "content": sys})
	}

	for _, m := range req.Messages {
		if err := appendAnthropicMessage(&messages, m); err != nil {
			return nil, err
		}
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("消息内容为空")
	}
	payload["messages"] = messages

	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if len(t.InputSchema) > 0 {
				var schema any
				if json.Unmarshal(t.InputSchema, &schema) == nil {
					fn["parameters"] = schema
				}
			}
			tool := map[string]any{"type": "function", "function": fn}
			if control := decodeCacheControl(t.CacheControl); control != nil {
				tool["cache_control"] = control
			}
			tools = append(tools, tool)
		}
		payload["tools"] = tools
		if tc := anthropicToolChoice(req.ToolChoice); tc != nil {
			payload["tool_choice"] = tc
		}
		var choice map[string]any
		if json.Unmarshal(req.ToolChoice, &choice) == nil {
			if disabled, ok := choice["disable_parallel_tool_use"].(bool); ok {
				payload["parallel_tool_calls"] = !disabled
			}
		}
	}
	return payload, nil
}

// appendAnthropicMessage 把一个 Anthropic 消息展开为一条或多条 OpenAI 消息。
func appendAnthropicMessage(messages *[]any, m anthropicMessage) error {
	role := strings.ToLower(strings.TrimSpace(m.Role))
	if role == "" {
		role = "user"
	}

	// content 为字符串时直接透传。
	var text string
	if json.Unmarshal(m.Content, &text) == nil {
		if role == "assistant" {
			*messages = append(*messages, map[string]any{"role": "assistant", "content": text})
		} else {
			*messages = append(*messages, map[string]any{"role": role, "content": text})
		}
		return nil
	}

	var blocks []map[string]any
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return fmt.Errorf("content 字段格式不支持")
	}

	if role == "assistant" {
		var textParts []string
		var cachedTextParts []any
		cacheMarked := false
		var toolCalls []any
		for _, b := range blocks {
			switch b["type"] {
			case "text":
				if t, ok := b["text"].(string); ok && t != "" {
					textParts = append(textParts, t)
					part := copyCacheControl(map[string]any{"type": "text", "text": t}, b)
					cachedTextParts = append(cachedTextParts, part)
					if part["cache_control"] != nil {
						cacheMarked = true
					}
				}
			case "tool_use":
				id, _ := b["id"].(string)
				name, _ := b["name"].(string)
				args, _ := json.Marshal(b["input"])
				toolCalls = append(toolCalls, map[string]any{
					"id":   id,
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": string(args),
					},
				})
			}
		}
		msg := map[string]any{"role": "assistant"}
		if len(textParts) > 0 {
			msg["content"] = strings.Join(textParts, "\n")
			if cacheMarked {
				msg["content"] = cachedTextParts
			}
		} else {
			msg["content"] = nil
		}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
		}
		if msg["content"] != nil || len(toolCalls) > 0 {
			*messages = append(*messages, msg)
		}
		return nil
	}

	// user：文本 / 图片合并为一条消息，tool_result 单独成为 role=tool 消息。
	var parts []any
	flush := func() {
		if len(parts) == 0 {
			return
		}
		*messages = append(*messages, map[string]any{"role": role, "content": parts})
		parts = nil
	}
	for _, b := range blocks {
		switch b["type"] {
		case "text":
			if t, ok := b["text"].(string); ok && t != "" {
				parts = append(parts, copyCacheControl(map[string]any{"type": "text", "text": t}, b))
			}
		case "image":
			if u := anthropicImageURL(b); u != "" {
				parts = append(parts, copyCacheControl(map[string]any{
					"type": "image_url", "image_url": map[string]any{"url": u},
				}, b))
			}
		case "document":
			if u := anthropicDocumentDataURL(b); u != "" {
				parts = append(parts, map[string]any{
					"type": "image_url", "image_url": map[string]any{"url": u},
				})
			} else {
				name := firstString(b["title"], b["name"])
				if name == "" {
					name = "document"
				}
				parts = append(parts, map[string]any{"type": "text", "text": "[document] " + name})
			}
		case "tool_result":
			flush()
			id, _ := b["tool_use_id"].(string)
			content := anthropicToolResultText(b)
			if content == "" {
				content = "(no output)"
			}
			*messages = append(*messages, copyCacheControl(map[string]any{
				"role": "tool", "tool_call_id": id, "content": content,
			}, b))
		}
	}
	flush()
	return nil
}

// anthropicSystemText 提取 system 字段（字符串或文本块数组）。
func anthropicSystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b["type"] == "text" {
			if t, ok := b["text"].(string); ok && t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func anthropicImageURL(block map[string]any) string {
	src, ok := block["source"].(map[string]any)
	if !ok {
		return ""
	}
	switch src["type"] {
	case "base64":
		media, _ := src["media_type"].(string)
		data, _ := src["data"].(string)
		if data == "" {
			return ""
		}
		if media == "" {
			media = "image/png"
		}
		return "data:" + media + ";base64," + data
	case "url":
		u, _ := src["url"].(string)
		return u
	}
	return ""
}

func anthropicDocumentDataURL(block map[string]any) string {
	src, ok := block["source"].(map[string]any)
	if !ok || src["type"] != "base64" {
		return ""
	}
	media, _ := src["media_type"].(string)
	if !strings.HasPrefix(media, "image/") {
		return ""
	}
	data, _ := src["data"].(string)
	if data == "" {
		return ""
	}
	return "data:" + media + ";base64," + data
}

func anthropicToolResultText(block map[string]any) string {
	switch c := block["content"].(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, item := range c {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch m["type"] {
			case "text":
				if t, ok := m["text"].(string); ok {
					parts = append(parts, t)
				}
			case "image":
				parts = append(parts, "[image omitted]")
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func anthropicToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var body struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return nil
	}
	switch body.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if body.Name == "" {
			return nil
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": body.Name}}
	}
	return nil
}

// --- OpenAI → Anthropic ---

// chatToAnthropicMessage 把 OpenAI chat 响应转换为 Anthropic message。
func chatToAnthropicMessage(out map[string]any, publicModel string) map[string]any {
	content := make([]any, 0, 3)
	stopReason := "end_turn"

	choices, _ := out["choices"].([]any)
	if len(choices) > 0 {
		if cm, ok := choices[0].(map[string]any); ok {
			if fr, ok := cm["finish_reason"].(string); ok && fr != "" {
				stopReason = anthropicStopReason(fr)
			}
			if msg, ok := cm["message"].(map[string]any); ok {
				if reasoning, ok := msg["reasoning"].(string); ok && reasoning != "" {
					content = append(content, map[string]any{
						"type": "thinking", "thinking": reasoning, "signature": "",
					})
				}
				if text, ok := msg["content"].(string); ok && text != "" {
					content = append(content, map[string]any{"type": "text", "text": text})
				}
				if tcs, ok := msg["tool_calls"].([]any); ok {
					for _, tcv := range tcs {
						tc, ok := tcv.(map[string]any)
						if !ok {
							continue
						}
						id, _ := tc["id"].(string)
						name := ""
						input := map[string]any{}
						if fn, ok := tc["function"].(map[string]any); ok {
							name, _ = fn["name"].(string)
							if args, ok := fn["arguments"].(string); ok && args != "" {
								var parsed map[string]any
								if json.Unmarshal([]byte(args), &parsed) == nil {
									input = parsed
								} else {
									input = map[string]any{"_raw": args}
								}
							}
						}
						content = append(content, map[string]any{
							"type": "tool_use", "id": id, "name": name, "input": input,
						})
					}
				}
			}
		}
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}

	usage := parseUsage(out["usage"])
	return map[string]any{
		"id":            "msg_" + deepai.RandomHex(12),
		"type":          "message",
		"role":          "assistant",
		"model":         publicModel,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         anthropicUsage(usage),
	}
}

// anthropicStopReason 映射 OpenAI finish_reason → Anthropic stop_reason。
func anthropicStopReason(fr string) string {
	switch fr {
	case "tool_calls", "function_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "end_turn"
	default:
		return "end_turn"
	}
}

func anthropicTextLen(msg map[string]any) int {
	total := 0
	content, _ := msg["content"].([]any)
	for _, c := range content {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		switch cm["type"] {
		case "text":
			total += lenOfString(cm["text"])
		case "thinking":
			total += lenOfString(cm["thinking"])
		}
	}
	return total
}

// --- Anthropic 流式内容块管理器 ---

type anthropicBlockWriter struct {
	send     func(event string, v any)
	kind     string // "" / thinking / text / tool
	index    int
	open     bool
	toolSeen map[int]bool
}

func newAnthropicBlockWriter(send func(string, any)) *anthropicBlockWriter {
	return &anthropicBlockWriter{send: send, index: -1, toolSeen: map[int]bool{}}
}

func (b *anthropicBlockWriter) openBlock(kind string, content map[string]any) {
	b.close()
	b.index++
	b.kind = kind
	b.open = true
	b.send("content_block_start", map[string]any{
		"type": "content_block_start", "index": b.index, "content_block": content,
	})
}

func (b *anthropicBlockWriter) close() {
	if !b.open {
		return
	}
	b.send("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": b.index,
	})
	b.open = false
	b.kind = ""
}

func (b *anthropicBlockWriter) thinking(text string) {
	if b.kind != "thinking" {
		b.openBlock("thinking", map[string]any{"type": "thinking", "thinking": "", "signature": ""})
	}
	b.send("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": b.index,
		"delta": map[string]any{"type": "thinking_delta", "thinking": text},
	})
}

func (b *anthropicBlockWriter) text(text string) {
	if b.kind != "text" {
		b.openBlock("text", map[string]any{"type": "text", "text": ""})
	}
	b.send("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": b.index,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

func (b *anthropicBlockWriter) toolCall(toolIdx int, id, name, args string) {
	if b.kind != "tool" || !b.toolSeen[toolIdx] {
		b.openBlock("tool", map[string]any{
			"type": "tool_use", "id": id, "name": name, "input": map[string]any{},
		})
		b.toolSeen[toolIdx] = true
	}
	if args != "" {
		b.send("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": b.index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
		})
	}
}

// --- 工具函数 ---

// estTokens 用字符数粗略估算 token（1 token ≈ 2 字符）。
func estTokens(chars int) int {
	if chars <= 0 {
		return 0
	}
	return chars/2 + 1
}

func firstString(vals ...any) string {
	for _, v := range vals {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// writeAnthropicErr 按 Anthropic 的错误结构返回。
func writeAnthropicErr(w http.ResponseWriter, code int, typ, msg string) {
	writeJSON(w, code, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
}

// requestTimeout 返回设置里的请求超时。
func (s *Server) requestTimeout() time.Duration {
	sec := s.Store.Settings().RequestTimeout
	if sec <= 0 {
		sec = 600
	}
	return time.Duration(sec) * time.Second
}

// ensure context import is used even if code changes later.
var _ = context.Background
var _ = io.Discard
