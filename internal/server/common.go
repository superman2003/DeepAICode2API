package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/registry"
	"deepaicode2api/internal/store"
)

// maxRequestBody 限制入站请求体大小。
const maxRequestBody = 32 << 20

// Usage 是归一化后的 token / 成本用量。
type Usage struct {
	PromptTokens          int
	CompletionTokens      int
	TotalTokens           int
	ReasoningTokens       int
	CacheReadTokens       int
	CacheCreationTokens   int
	CacheCreation5mTokens int
	CacheCreation1hTokens int
	Cost                  float64
}

// Add 累加用量。
func (u *Usage) Add(v Usage) {
	u.PromptTokens += v.PromptTokens
	u.CompletionTokens += v.CompletionTokens
	u.TotalTokens += v.TotalTokens
	u.ReasoningTokens += v.ReasoningTokens
	u.CacheReadTokens += v.CacheReadTokens
	u.CacheCreationTokens += v.CacheCreationTokens
	u.CacheCreation5mTokens += v.CacheCreation5mTokens
	u.CacheCreation1hTokens += v.CacheCreation1hTokens
	u.Cost += v.Cost
}

// affinityPattern 与桌面端一致：只透传安全字符集的会话标识。
var affinityPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,256}$`)

// affinityFor 解析会话粘性标识（响应头优先，其次 prompt_cache_key）。
func affinityFor(r *http.Request, payload map[string]any) string {
	if r != nil {
		if v := strings.TrimSpace(r.Header.Get("X-Session-Affinity")); affinityPattern.MatchString(v) {
			return v
		}
	}
	if payload != nil {
		if v, ok := payload["prompt_cache_key"].(string); ok && affinityPattern.MatchString(strings.TrimSpace(v)) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// resolveModel 用当前目录 + 别名解析请求模型。
func (s *Server) resolveModel(name string) deepai.Model {
	catalog := s.Store.Models()
	if len(catalog) == 0 {
		catalog = []deepai.Model{deepai.DefaultModelEntry()}
	}
	settings := s.Store.Settings()
	def := s.Store.DefaultModel()
	if settings.FallbackModel != "" {
		if m, ok := registry.Find(catalog, settings.FallbackModel); ok {
			def = m.ID
		} else {
			def = settings.FallbackModel
		}
	}
	return registry.Resolve(catalog, settings.ModelAliases, name, def)
}

// openModel 在账号池上打开一次上游推理请求。
//
// 语义：
//   - 2xx：立即返回响应，调用方负责关闭 Body；
//   - 400/404/422 等请求错误：不重试，直接返回 *deepai.HTTPError；
//   - 401/403/429/5xx：记录账号失败并换号重试；
//   - 409 session_busy：按 Retry-After 等待后重试。
func (s *Server) openModel(ctx context.Context, path string, body []byte, affinity, group string) (*http.Response, *deepai.Account, error) {
	accts := s.Store.Accounts()
	if len(accts) == 0 {
		return nil, nil, errors.New("没有可用账号，请先在控制台导入桌面端凭据或添加 API Key")
	}
	settings := s.Store.Settings()
	maxTries := settings.MaxRetries
	if maxTries <= 0 {
		maxTries = 3
	}
	if maxTries > len(accts)+2 {
		maxTries = len(accts) + 2
	}

	var (
		lastErr error
		used    *deepai.Account
	)
	for i := 0; i < maxTries; i++ {
		var acct *deepai.Account
		if i == 0 && affinity != "" {
			acct = s.cachedSessionAccount(ctx, group, affinity)
		}
		if acct == nil {
			acct = s.Store.NextAccountGroup(group)
		}
		if acct == nil {
			break
		}
		used = acct

		resp, err := s.clientFor(acct).OpenModel(ctx, acct, path, body, affinity)
		if err != nil {
			lastErr = err
			s.Store.MarkAccountResult(acct.ID, false, err.Error())
			if ctx.Err() != nil {
				break
			}
			continue
		}

		if resp.StatusCode/100 == 2 {
			s.Store.MarkAccountResult(acct.ID, true, "")
			if affinity != "" {
				s.rememberSessionAccount(ctx, group, affinity, acct.ID)
			}
			return resp, acct, nil
		}

		payload, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			s.Store.MarkAccountResult(acct.ID, false, readErr.Error())
			continue
		}
		httpErr := &deepai.HTTPError{Op: "上游推理", Status: resp.StatusCode, Body: payload}

		// 容量拒绝：按上游建议等待后重试（不换号，保持会话粘性）。
		if httpErr.CapacityWaiting() {
			lastErr = httpErr
			wait := deepai.RetryAfter(resp, 2*time.Second)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, acct, ctx.Err()
			}
			continue
		}

		// 请求本身的问题：直接回传，不浪费其他账号。
		if !httpErr.Retryable() {
			return nil, acct, httpErr
		}

		lastErr = httpErr
		s.Store.MarkAccountResult(acct.ID, false, httpErr.Error())
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = errors.New("全部账号均不可用")
	}
	return nil, used, lastErr
}

// callJSON 发起一次非流式上游请求并解析 JSON。
func (s *Server) callJSON(ctx context.Context, path string, payload map[string]any, affinity, group string) (map[string]any, *deepai.Account, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("请求序列化失败: %w", err)
	}
	resp, acct, err := s.openModel(ctx, path, body, affinity, group)
	if err != nil {
		return nil, acct, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, acct, fmt.Errorf("读取上游响应失败: %w", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, acct, fmt.Errorf("上游响应解析失败: %w", err)
	}
	return out, acct, nil
}

// writeUpstreamErr 把上游错误原样（或包装后）返回给客户端。
func writeUpstreamErr(w http.ResponseWriter, err error) {
	var he *deepai.HTTPError
	if errors.As(err, &he) {
		if json.Valid(he.Body) && len(he.Body) > 0 {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(he.Status)
			_, _ = w.Write(he.Body)
			return
		}
		writeErr(w, he.Status, he.Error(), "upstream_error")
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeErr(w, http.StatusGatewayTimeout, "上游请求超时", "upstream_error")
		return
	}
	writeErr(w, http.StatusBadGateway, err.Error(), "upstream_error")
}

// writeAnthropicUpstreamErr 按 Anthropic 错误结构回传上游错误。
func writeAnthropicUpstreamErr(w http.ResponseWriter, err error) {
	var he *deepai.HTTPError
	if errors.As(err, &he) {
		typ := "api_error"
		switch he.Status {
		case http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity:
			typ = "invalid_request_error"
		case http.StatusUnauthorized, http.StatusForbidden:
			typ = "authentication_error"
		case http.StatusTooManyRequests:
			typ = "rate_limit_error"
		}
		writeAnthropicErr(w, he.Status, typ, he.Error())
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	writeAnthropicErr(w, http.StatusBadGateway, "api_error", err.Error())
}

// parseUsage 兼容 chat/completions 与 responses 两种用量字段。
func parseUsage(v any) Usage {
	m, ok := v.(map[string]any)
	if !ok {
		return Usage{}
	}
	u := Usage{
		PromptTokens:     intOf(m["prompt_tokens"]),
		CompletionTokens: intOf(m["completion_tokens"]),
		TotalTokens:      intOf(m["total_tokens"]),
		Cost:             floatOf(m["cost"]),
	}
	if u.PromptTokens == 0 {
		u.PromptTokens = intOf(m["input_tokens"])
	}
	if u.CompletionTokens == 0 {
		u.CompletionTokens = intOf(m["output_tokens"])
	}
	if u.TotalTokens == 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	if d, ok := m["completion_tokens_details"].(map[string]any); ok {
		u.ReasoningTokens = intOf(d["reasoning_tokens"])
	}
	if d, ok := m["output_tokens_details"].(map[string]any); ok && u.ReasoningTokens == 0 {
		u.ReasoningTokens = intOf(d["reasoning_tokens"])
	}
	u.CacheReadTokens = intOf(m["cache_read_input_tokens"])
	u.CacheCreationTokens = intOf(m["cache_creation_input_tokens"])
	for _, name := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if details, ok := m[name].(map[string]any); ok {
			if u.CacheReadTokens == 0 {
				u.CacheReadTokens = intOf(details["cached_tokens"])
			}
			if u.CacheCreationTokens == 0 {
				u.CacheCreationTokens = intOf(details["cache_write_tokens"])
			}
		}
	}
	if creation, ok := m["cache_creation"].(map[string]any); ok {
		u.CacheCreation5mTokens = intOf(creation["ephemeral_5m_input_tokens"])
		u.CacheCreation1hTokens = intOf(creation["ephemeral_1h_input_tokens"])
		if u.CacheCreationTokens == 0 {
			u.CacheCreationTokens = u.CacheCreation5mTokens + u.CacheCreation1hTokens
		}
	}
	// Anthropic input_tokens excludes cache reads/writes; OpenAI input totals
	// already include them. Keep one total internally to avoid double counting.
	_, hasChatTotal := m["prompt_tokens"]
	_, hasResponsesDetails := m["input_tokens_details"]
	_, hasAnthropicRead := m["cache_read_input_tokens"]
	_, hasAnthropicWrite := m["cache_creation_input_tokens"]
	if !hasChatTotal && !hasResponsesDetails && (hasAnthropicRead || hasAnthropicWrite) {
		u.PromptTokens += u.CacheReadTokens + u.CacheCreationTokens
		if intOf(m["total_tokens"]) == 0 {
			u.TotalTokens = u.PromptTokens + u.CompletionTokens
		}
	}
	if c, ok := m["cost_details"].(map[string]any); ok && u.Cost == 0 {
		u.Cost = floatOf(c["upstream_inference_cost"])
	}
	return u
}

// Streaming providers may split input/cache usage and output usage into separate
// chunks. Update only the counters actually present in the new frame.
func mergeStreamUsage(current Usage, value any) Usage {
	m, ok := value.(map[string]any)
	if !ok {
		return current
	}
	next := parseUsage(m)
	present := func(names ...string) bool {
		for _, name := range names {
			if v, ok := m[name]; ok && v != nil {
				return true
			}
		}
		return false
	}
	detail := func(field string, names ...string) bool {
		for _, name := range names {
			if nested, ok := m[name].(map[string]any); ok {
				if v, ok := nested[field]; ok && v != nil {
					return true
				}
			}
		}
		return false
	}
	oldRead, oldWrite := current.CacheReadTokens, current.CacheCreationTokens
	if present("cache_read_input_tokens") || detail("cached_tokens", "prompt_tokens_details", "input_tokens_details") {
		current.CacheReadTokens = next.CacheReadTokens
	}
	if present("cache_creation_input_tokens", "cache_creation") || detail("cache_write_tokens", "prompt_tokens_details", "input_tokens_details") {
		current.CacheCreationTokens = next.CacheCreationTokens
		current.CacheCreation5mTokens = next.CacheCreation5mTokens
		current.CacheCreation1hTokens = next.CacheCreation1hTokens
	}
	if present("prompt_tokens", "input_tokens") {
		current.PromptTokens = next.PromptTokens
	} else if present("cache_read_input_tokens", "cache_creation_input_tokens") {
		current.PromptTokens += current.CacheReadTokens - oldRead + current.CacheCreationTokens - oldWrite
	}
	if present("completion_tokens", "output_tokens") {
		current.CompletionTokens = next.CompletionTokens
	}
	if detail("reasoning_tokens", "completion_tokens_details", "output_tokens_details") {
		current.ReasoningTokens = next.ReasoningTokens
	}
	if present("cost", "cost_details") {
		current.Cost = next.Cost
	}
	if present("total_tokens") {
		current.TotalTokens = next.TotalTokens
	} else {
		current.TotalTokens = current.PromptTokens + current.CompletionTokens
	}
	return current
}

func intOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

func floatOf(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

// record 记录一次请求元信息。
func (s *Server) record(key string, acct *deepai.Account, model, endpoint string, stream bool, status int, start time.Time, inLen, outLen int, usage Usage, err error) {
	e := store.LogEntry{
		Time:                time.Now().Unix(),
		Key:                 mask(key),
		Model:               model,
		Endpoint:            endpoint,
		Stream:              stream,
		Status:              status,
		LatencyMS:           time.Since(start).Milliseconds(),
		PromptLen:           inLen,
		OutputLen:           outLen,
		TokensIn:            usage.PromptTokens,
		TokensOut:           usage.CompletionTokens,
		CacheReadTokens:     usage.CacheReadTokens,
		CacheCreationTokens: usage.CacheCreationTokens,
		Cost:                usage.Cost,
	}
	if acct != nil {
		e.Account = acct.ID
	}
	if err != nil {
		e.Err = err.Error()
	}
	s.Store.Record(e, outLen)
}

// readBodyWithLimit 读取入站请求体。
func readBodyWithLimit(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
}

// requestModelName 提取请求里的 model 字段。
func requestModelName(payload map[string]any) string {
	v, _ := payload["model"].(string)
	return strings.TrimSpace(v)
}

// estimatePromptLen 粗略统计 messages / input 的字符数。
func estimatePromptLen(payload map[string]any) int {
	if v, ok := payload["messages"]; ok {
		if raw, err := json.Marshal(v); err == nil {
			return len(raw)
		}
	}
	if v, ok := payload["input"]; ok {
		if raw, err := json.Marshal(v); err == nil {
			return len(raw)
		}
	}
	return 0
}

// chatTextLen 统计 chat 响应中的正文 + 思维链字符数。
func chatTextLen(m map[string]any) int {
	choices, _ := m["choices"].([]any)
	total := 0
	for _, c := range choices {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if msg, ok := cm["message"].(map[string]any); ok {
			total += lenOfString(msg["content"])
			total += lenOfString(msg["reasoning"])
		}
		if d, ok := cm["delta"].(map[string]any); ok {
			total += lenOfString(d["content"])
			total += lenOfString(d["reasoning"])
		}
	}
	return total
}

func lenOfString(v any) int {
	s, _ := v.(string)
	return len([]rune(s))
}

// writeRawJSON 直接写出已生成的 JSON 字节。
func writeRawJSON(w http.ResponseWriter, code int, raw []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(raw)
}

// sseWriter 统一 SSE 响应头与 flush。
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	flusher, _ := w.(http.Flusher)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	if flusher != nil {
		flusher.Flush()
	}
	return &sseWriter{w: w, flusher: flusher}
}

func (s *sseWriter) data(raw []byte) {
	_ = deepai.WriteSSE(s.w, raw)
	s.flush()
}

func (s *sseWriter) event(name string, raw []byte) {
	_ = deepai.WriteSSEEvent(s.w, name, raw)
	s.flush()
}

func (s *sseWriter) done() {
	_, _ = s.w.Write([]byte("data: [DONE]\n\n"))
	s.flush()
}

func (s *sseWriter) flush() {
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// readSSELines 逐行读取上游 SSE；回调返回 false 时停止。
func readSSELines(body io.Reader, fn func(line string) bool) error {
	br := bufio.NewReaderSize(body, 64*1024)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			if !fn(line) {
				return nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// jsonEvent 序列化事件载荷，失败时返回 nil。
func jsonEvent(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return raw
}

// requestGroup 返回当前请求密钥绑定的号池分组（无密钥时为空=全部分组）。
func (s *Server) requestGroup(r *http.Request) string {
	if r == nil {
		return ""
	}
	key := extractKey(r)
	if key == "" {
		return ""
	}
	return s.Store.KeyGroup(key)
}

// nextAccount 按请求所属分组挑一个轮询账号。
func (s *Server) nextAccount(r *http.Request) *deepai.Account {
	return s.Store.NextAccountGroup(s.requestGroup(r))
}

// clientFor 返回账号绑定的上游客户端（按代理缓存）。
func (s *Server) clientFor(acct *deepai.Account) *deepai.Client {
	if acct == nil || strings.TrimSpace(acct.Proxy) == "" {
		return s.Up
	}
	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()
	if s.proxyClients == nil {
		s.proxyClients = map[string]*deepai.Client{}
	}
	if c, ok := s.proxyClients[acct.Proxy]; ok {
		return c
	}
	c := deepai.NewClientWithProxy(acct.Proxy)
	s.proxyClients[acct.Proxy] = c
	return c
}
