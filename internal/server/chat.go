package server

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"deepaicode2api/internal/deepai"
)

// handleChatCompletions 是 /v1/chat/completions 的主入口。
//
// 上游本身就是 OpenAI 兼容接口，因此本网关做的是“带账号池与配额治理的
// 透明代理”：改写模型名、注入 DeepAI 鉴权头、归一化思维链字段、记录用量。
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
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
	if msgs, ok := payload["messages"].([]any); !ok || len(msgs) == 0 {
		writeErr(w, http.StatusBadRequest, "messages 不能为空", "invalid_request_error")
		return
	}

	model := s.resolveModel(requestModelName(payload))
	payload["model"] = model.ID
	stream, _ := payload["stream"].(bool)
	// 必须显式下发 stream=false：上游在缺省该字段时默认按 SSE 返回，
	// 而 OpenAI 规范里缺省等价于 false，两者不一致会导致非流式请求拿到
	// `data: {...}` 无法解析。
	payload["stream"] = stream
	affinity := affinityFor(r, payload)
	group := s.requestGroup(r)

	body, err := json.Marshal(payload)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "请求序列化失败", "invalid_request_error")
		return
	}
	key := extractKey(r)
	promptLen := estimatePromptLen(payload)

	resp, acct, err := s.openModel(r.Context(), deepai.PathChat, body, affinity, group)
	if err != nil {
		s.record(key, acct, model.ID, "chat", stream, statusOf(err), start, promptLen, 0, Usage{}, err)
		writeUpstreamErr(w, err)
		return
	}
	defer resp.Body.Close()

	if stream {
		s.streamChat(w, resp.Body, model.ID, key, acct, start, promptLen)
		return
	}

	rawResp, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		s.record(key, acct, model.ID, "chat", false, http.StatusBadGateway, start, promptLen, 0, Usage{}, err)
		writeErr(w, http.StatusBadGateway, "读取上游响应失败: "+err.Error(), "upstream_error")
		return
	}
	var out map[string]any
	if err := json.Unmarshal(rawResp, &out); err != nil {
		// 上游偶尔会回非 JSON（HTML 网关页、纯文本限流提示、SSE 帧等）。
		// 把响应类型与开头片段写进日志，否则只剩“不是合法 JSON”无从下手。
		log.Printf("上游响应解析失败：account=%s status=%d ct=%q len=%d head=%q",
			acctLabel(acct), resp.StatusCode, resp.Header.Get("Content-Type"), len(rawResp), headOf(rawResp, 160))
		s.record(key, acct, model.ID, "chat", false, http.StatusBadGateway, start, promptLen, 0, Usage{}, err)
		writeErr(w, http.StatusBadGateway, "上游响应不是合法 JSON", "upstream_error")
		return
	}
	transformChatResponse(out, model.ID)
	usage := parseUsage(out["usage"])
	outLen := chatTextLen(out)

	encoded, err := json.Marshal(out)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "响应序列化失败", "server_error")
		return
	}
	s.record(key, acct, model.ID, "chat", false, http.StatusOK, start, promptLen, outLen, usage, nil)
	writeRawJSON(w, http.StatusOK, encoded)
}

// transformChatResponse 归一化非流式响应。
//
// 上游把思维链放在 `reasoning`；这里补一份 OpenAI 生态常用的
// `reasoning_content`（已存在则不覆盖），并把 model 改回对外模型名。
func transformChatResponse(m map[string]any, publicModel string) {
	if publicModel != "" {
		m["model"] = publicModel
	}
	choices, _ := m["choices"].([]any)
	for _, c := range choices {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if msg, ok := cm["message"].(map[string]any); ok {
			addReasoningContent(msg)
		}
	}
}

// transformChatChunk 归一化单个流式 chunk。
func transformChatChunk(m map[string]any, publicModel string) {
	if publicModel != "" {
		m["model"] = publicModel
	}
	choices, _ := m["choices"].([]any)
	for _, c := range choices {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if d, ok := cm["delta"].(map[string]any); ok {
			addReasoningContent(d)
		}
	}
}

// addReasoningContent 在 reasoning 非空且 reasoning_content 缺失时补字段。
func addReasoningContent(obj map[string]any) {
	if _, exists := obj["reasoning_content"]; exists {
		return
	}
	if v, ok := obj["reasoning"].(string); ok && v != "" {
		obj["reasoning_content"] = v
	}
}

// streamChat 以 OpenAI SSE 格式流式转发。
func (s *Server) streamChat(w http.ResponseWriter, body io.Reader, publicModel, key string, acct *deepai.Account, start time.Time, promptLen int) {
	sw := newSSEWriter(w)
	w.WriteHeader(http.StatusOK)

	var (
		usage  Usage
		outLen int
	)

	streamErr := readSSELines(body, func(line string) bool {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			return true
		}
		if strings.HasPrefix(trimmed, ":") {
			// 上游心跳，原样透传以维持客户端连接。
			_, _ = w.Write([]byte(trimmed + "\n\n"))
			sw.flush()
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
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// 无法解析时按原文透传，避免破坏上游扩展事件。
			_, _ = w.Write([]byte("data: " + data + "\n\n"))
			sw.flush()
			return true
		}
		transformChatChunk(chunk, publicModel)
		usage = mergeStreamUsage(usage, chunk["usage"])
		outLen += chatTextLen(chunk)
		if raw := jsonEvent(chunk); raw != nil {
			sw.data(raw)
		}
		return true
	})

	if streamErr != nil {
		errPayload := jsonEvent(map[string]any{
			"error": map[string]any{"message": streamErr.Error(), "type": "upstream_error"},
		})
		if errPayload != nil {
			sw.data(errPayload)
		}
	}
	sw.done()
	s.record(key, acct, publicModel, "chat", true, statusOf(streamErr), start, promptLen, outLen, usage, streamErr)
}

// acctLabel 返回用于日志的账号标识（不含密钥）。
func acctLabel(a *deepai.Account) string {
	if a == nil {
		return "-"
	}
	return a.Name
}

// headOf 截取响应开头片段用于排障。
func headOf(raw []byte, n int) string {
	if len(raw) <= n {
		return string(raw)
	}
	return string(raw[:n]) + "…"
}
