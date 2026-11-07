package deepai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"deepaicode2api/internal/proxychain"
)

// maxJSONBytes 限制上游 JSON 响应体大小（模型目录 / 配额 / 搜索）。
const maxJSONBytes = 8 << 20

// HTTPError 是上游返回的非 2xx 响应。
type HTTPError struct {
	Op     string
	Status int
	Body   []byte
}

// Error 返回可读错误信息（优先使用上游 error.message）。
func (e *HTTPError) Error() string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(e.Body, &payload) == nil && payload.Error.Message != "" {
		return fmt.Sprintf("%s: %s", e.Op, payload.Error.Message)
	}
	body := strings.TrimSpace(string(e.Body))
	if len(body) > 200 {
		body = body[:200] + "…"
	}
	if body == "" {
		return fmt.Sprintf("%s: HTTP %d", e.Op, e.Status)
	}
	return fmt.Sprintf("%s: HTTP %d: %s", e.Op, e.Status, body)
}

// Code 返回上游 error.code。
func (e *HTTPError) Code() string {
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(e.Body, &payload) == nil {
		return payload.Error.Code
	}
	return ""
}

// Retryable 报告该错误是否值得换账号 / 重试。
//
// 400 / 404 / 422 是请求本身的问题，重试无意义；
// 401 / 403 / 409 / 429 / 5xx 与账号或容量相关，可以重试。
func (e *HTTPError) Retryable() bool {
	switch e.Status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict,
		http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return e.Status >= 500
}

// AuthFailed 报告是否为账号鉴权失败。
func (e *HTTPError) AuthFailed() bool {
	if e.Status == http.StatusUnauthorized {
		return true
	}
	code := e.Code()
	return code == "auth_required" || code == "invalid_api_key" || code == "email_unverified"
}

// CapacityWaiting 报告是否为“会话忙”的容量拒绝。
func (e *HTTPError) CapacityWaiting() bool {
	return e.Status == http.StatusConflict && e.Code() == "session_busy"
}

// RetryAfter 解析 Retry-After 头（秒），默认 fallback。
func RetryAfter(resp *http.Response, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if raw == "" {
		return fallback
	}
	var secs int
	if _, err := fmt.Sscanf(raw, "%d", &secs); err != nil || secs <= 0 {
		return fallback
	}
	if secs > 30 {
		secs = 30
	}
	return time.Duration(secs) * time.Second
}

// transientMarkers 是已知的“出口/网络抖动”错误特征。
//
// 有些代理会在隧道建立阶段返回自定义文案（例如 "No exit node"），
// 它既不是 net.Error 也不包装 io.EOF，只能按文案识别。
var transientMarkers = []string{
	"no exit node",
	"unexpected eof",
	"tls handshake timeout",
	"connection reset",
	"connection refused",
	"broken pipe",
	"i/o timeout",
	"timeout awaiting response",
	"temporary failure in name resolution",
}

// IsTransient 报告错误是否属于“再试一次可能成功”的瞬时故障。
//
// 用于 /theos 验活、入池探测这类关键路径的退避重试判断：
// 网络抖动与 5xx/429 值得重试；4xx 是请求或账号本身的问题，重试无意义。
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	// 调用方主动取消不算可重试。
	if errors.Is(err, context.Canceled) {
		return false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		switch he.Status {
		case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
			return true
		}
		return he.Status >= 500
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range transientMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// Client 是 DeepAI 上游客户端。
type Client struct {
	Origin    string
	HTTP      *http.Client
	Machine   string
	UserAgent string
	Proxy     string
	protocol  *laneProtocol
}

// NewClient 构造上游客户端（默认读取环境代理）。
//
// DEEPAI_API_ORIGIN 可覆盖上游地址（仅接受 http/https）。
func NewClient() *Client { return NewClientWithFingerprint("", "") }

// NewClientWithProxy 构造走指定代理的上游客户端。
func NewClientWithProxy(proxyRaw string) *Client {
	return NewClientWithFingerprint(proxyRaw, "")
}

// NewClientWithFingerprint 构造带代理与 User-Agent 的上游客户端。
func NewClientWithFingerprint(proxyRaw, userAgent string) *Client {
	origin := strings.TrimRight(EnvOr("DEEPAI_API_ORIGIN", DefaultOrigin), "/")
	if u, err := url.Parse(origin); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		origin = DefaultOrigin
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 2 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	proxyRaw = strings.TrimSpace(proxyRaw)
	var explicitProxy *url.URL
	if proxyRaw != "" {
		if u, err := url.Parse(proxyRaw); err == nil && u.Host != "" {
			explicitProxy = u
			transport.Proxy = http.ProxyURL(u)
			proxychain.Configure(transport, u)
		}
	}
	return &Client{
		Origin:    origin,
		Machine:   MachineID(),
		UserAgent: strings.TrimSpace(userAgent),
		Proxy:     proxyRaw,
		protocol:  processLaneProtocol,
		HTTP: &http.Client{
			Transport: proxychain.WithDirectFallback(transport, explicitProxy),
			// 上游会 302 跳到登录页，跟随只会掩盖鉴权失败。
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// ModelURL 返回 /deepai_code 下的完整地址。
func (c *Client) ModelURL(path string) string { return c.Origin + ModelBasePath + path }

// RootURL 返回根路径完整地址。
func (c *Client) RootURL(path string) string { return c.Origin + path }

// SignInURL 返回桌面端同款浏览器登录地址。
func (c *Client) SignInURL(state string, port int) string {
	u := c.ModelURL(PathSignIn)
	q := url.Values{}
	q.Set("state", state)
	q.Set("port", fmt.Sprintf("%d", port))
	return u + "?" + q.Encode()
}

// commonHeaders 写入所有上游请求的公共头。
func (c *Client) commonHeaders(req *http.Request, acct *Account) {
	req.Header.Set(HeaderLane, LaneVersion)
	req.Header.Set(HeaderInstance, c.laneProtocol().instance)
	// 机器指纹优先取账号级：上游按它发放每日免费额度，共用会互相抵消。
	machine := c.Machine
	if acct != nil && acct.MachineID != "" {
		if fp := MachineFingerprint(acct.MachineID); fp != "" {
			machine = fp
		}
	}
	if machine != "" {
		req.Header.Set(HeaderMachine, machine)
	}
	req.Header.Set("Accept", "application/json")

	// 指纹头也优先取账号级：注册与推理必须用同一套特征。
	ua := c.UserAgent
	if acct != nil && acct.UserAgent != "" {
		ua = acct.UserAgent
	}
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if acct != nil {
		if acct.AcceptLanguage != "" {
			req.Header.Set("Accept-Language", acct.AcceptLanguage)
		}
		if acct.SecCHUA != "" {
			req.Header.Set("sec-ch-ua", acct.SecCHUA)
			req.Header.Set("sec-ch-ua-mobile", "?0")
		}
		if acct.Platform != "" {
			req.Header.Set("sec-ch-ua-platform", acct.Platform)
		}
	}
	if acct != nil && acct.APIKey != "" {
		req.Header.Set(HeaderKeyName, acct.APIKey)
	}
}

// do 发送请求并返回原样响应（调用方负责关闭 Body）。
func (c *Client) do(req *http.Request) (*http.Response, error) {
	return c.HTTP.Do(req)
}

// FetchModels 拉取上游模型目录（无需 API Key，但需要机器指纹）。
func (c *Client) FetchModels(ctx context.Context) ([]Model, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ModelURL(PathModels), nil)
	if err != nil {
		return nil, "", err
	}
	c.commonHeaders(req, nil)

	resp, err := c.do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes))
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode/100 != 2 {
		return nil, "", &HTTPError{Op: "模型目录", Status: resp.StatusCode, Body: body}
	}

	var payload struct {
		Models  []Model `json:"models"`
		Default string  `json:"default"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "", fmt.Errorf("模型目录解析失败: %w", err)
	}
	models := make([]Model, 0, len(payload.Models))
	seen := map[string]bool{}
	for _, m := range payload.Models {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		if m.Input == nil {
			m.Input = []string{"text"}
		}
		models = append(models, m)
	}
	if len(models) == 0 {
		return nil, "", errors.New("上游模型目录为空")
	}
	def := payload.Default
	if def == "" {
		def = models[0].ID
	}
	return models, def, nil
}

// OpenModel 发起一次推理请求，返回未经读取的响应。
//
// path 取 PathChat 或 PathResponses；affinity 会作为 X-Session-Affinity
// 透传（上游据此做会话粘性调度）。
func (c *Client) OpenModel(ctx context.Context, acct *Account, path string, body []byte, affinity string) (*http.Response, error) {
	if path == PathChat {
		body = normalizeChatInstructions(body)
	}
	c.checkInBeforeModel(ctx, acct)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ModelURL(path), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.commonHeaders(req, acct)
	if affinity != "" {
		req.Header.Set(HeaderAffinity, affinity)
	}
	return c.do(req)
}

// Theos 查询账号的免费额度 / 余额。
func (c *Client) Theos(ctx context.Context, acct *Account) (*Allowance, error) {
	state := c.checkInState(acct)
	select {
	case state.gate <- struct{}{}:
		defer func() { <-state.gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ModelURL(PathTheos), nil)
	if err != nil {
		return nil, err
	}
	c.commonHeaders(req, acct)
	c.checkInHeaders(req, state)

	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes))
	if err != nil {
		return nil, err
	}
	// Lane 4 can rotate the next nonce on both successful and refused check-ins.
	state.note(body, time.Now())
	if resp.StatusCode/100 != 2 {
		return nil, &HTTPError{Op: "查询 Theos", Status: resp.StatusCode, Body: body}
	}
	var out Allowance
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("Theos 响应解析失败: %w", err)
	}
	out.CheckedAt = time.Now().Unix()
	return &out, nil
}

// UsageSummary 查询账号订阅 / 用量。
func (c *Client) UsageSummary(ctx context.Context, acct *Account) (*UsageSummary, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.RootURL(PathUsageSummary), nil)
	if err != nil {
		return nil, err
	}
	c.commonHeaders(req, acct)
	req.Header.Set("User-Agent", "DeepAI-Code")

	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, &HTTPError{Op: "查询用量", Status: resp.StatusCode, Body: body}
	}
	var out UsageSummary
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("用量响应解析失败: %w", err)
	}
	return &out, nil
}

// WebSearch 调用上游联网搜索能力。
func (c *Client) WebSearch(ctx context.Context, acct *Account, payload []byte) ([]byte, error) {
	return c.postJSON(ctx, acct, PathWebSearch, payload, "联网搜索")
}

// FindService 调用上游服务推荐能力。
func (c *Client) FindService(ctx context.Context, acct *Account, payload []byte) ([]byte, error) {
	return c.postJSON(ctx, acct, PathFindService, payload, "服务搜索")
}

func (c *Client) postJSON(ctx context.Context, acct *Account, path string, payload []byte, op string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ModelURL(path), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.commonHeaders(req, acct)

	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, &HTTPError{Op: op, Status: resp.StatusCode, Body: body}
	}
	return body, nil
}

// Transcribe 把音频转发到上游语音转文字接口。
func (c *Client) Transcribe(ctx context.Context, acct *Account, filename, contentType string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", errors.New("音频内容为空")
	}
	if len(data) > 8<<20 {
		return "", errors.New("音频超过 8 MB")
	}
	if filename == "" {
		filename = "rec.webm"
	}
	if contentType == "" {
		contentType = "audio/webm"
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="voiceRecording"; filename="%s"`, escapeQuotes(filename)))
	h.Set("Content-Type", contentType)
	part, err := mw.CreatePart(h)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.RootURL(PathSpeechToText), &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if acct != nil {
		req.Header.Set("api-key", acct.APIKey)
	}

	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		return "", &HTTPError{Op: "语音转写", Status: resp.StatusCode, Body: body}
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("转写响应解析失败: %w", err)
	}
	if strings.TrimSpace(out.Text) == "" {
		return "", errors.New("上游未返回转写文本")
	}
	return strings.TrimSpace(out.Text), nil
}

// ExchangeCode 用桌面端登录回调里的 code 换取 API Key。
func (c *Client) ExchangeCode(ctx context.Context, code string) (*Credentials, error) {
	payload, _ := json.Marshal(map[string]string{"code": code})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ModelURL(PathExchange), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, &HTTPError{Op: "登录换取密钥", Status: resp.StatusCode, Body: body}
	}
	var out struct {
		APIKey   string `json:"api_key"`
		Username string `json:"username"`
		IsPro    bool   `json:"is_pro"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("登录响应解析失败: %w", err)
	}
	if out.APIKey == "" || strings.ContainsAny(out.APIKey, "\r\n") {
		return nil, errors.New("登录响应缺少有效 API Key")
	}
	return &Credentials{APIKey: out.APIKey, Username: out.Username, IsPro: out.IsPro}, nil
}

func escapeQuotes(s string) string {
	return strings.NewReplacer("\\", "\\\\", `"`, "\\\"").Replace(s)
}
