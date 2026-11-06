package register

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"deepaicode2api/internal/proxychain"
)

// DeepAI 网页端 API 源。
const (
	WebOrigin  = "https://api.deepai.org"
	SiteOrigin = "https://deepai.org"
)

// WebUser 是 /daily-time-sync/user/ 的返回。
type WebUser struct {
	PK       int64  `json:"pk"`
	Username string `json:"username"`
	Email    string `json:"email"`

	UserProfile struct {
		APIKey string `json:"api_key"`
	} `json:"userprofile"`

	UserProfile2 struct {
		Plan                        string `json:"plan"`
		UserHasDeepAIPro            bool   `json:"user_has_deepai_pro"`
		LockedOutDueToNoPaymentInfo bool   `json:"locked_out_due_to_no_payment_info"`
		UserCanUseGeniusMode        bool   `json:"user_can_use_genius_mode"`
		UserIsPayAsYouGo            bool   `json:"user_is_pay_as_you_go"`
	} `json:"userprofile2"`
}

// WebError 是网页端接口错误。
type WebError struct {
	Op      string
	Status  int
	Message string
}

func (e *WebError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("%s: HTTP %d: %s", e.Op, e.Status, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Op, e.Message)
}

// WebClient 是带 Cookie 会话的 DeepAI 网页客户端。
type WebClient struct {
	Origin      string
	Site        string
	UA          string
	Lang        string
	Fingerprint Fingerprint
	Proxy       string
	HTTP        *http.Client
	Jar         http.CookieJar
}

// NewWebClient 构造一个独立的网页会话（每个账号一个）。
func NewWebClient(cfg *Config) *WebClient {
	if cfg == nil {
		cfg = LoadConfig()
	}
	return NewWebClientWithFingerprint(cfg, AssignFingerprint("default"), cfg.Proxy)
}

// NewWebClientWithFingerprint 构造带指纹与代理的网页会话。
func NewWebClientWithFingerprint(cfg *Config, fp Fingerprint, proxy string) *WebClient {
	if cfg == nil {
		cfg = LoadConfig()
	}
	jar, _ := cookiejar.New(nil)
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 2 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	proxy = strings.TrimSpace(proxy)
	var explicitProxy *url.URL
	if proxy != "" {
		if u, err := url.Parse(proxy); err == nil && u.Host != "" {
			explicitProxy = u
			transport.Proxy = http.ProxyURL(u)
			proxychain.Configure(transport, u)
		}
	}
	ua := fp.UserAgent
	if ua == "" {
		ua = cfg.UserAgent
	}
	lang := fp.Language
	if lang == "" {
		lang = cfg.Language
	}
	return &WebClient{
		Origin:      WebOrigin,
		Site:        SiteOrigin,
		UA:          ua,
		Lang:        lang,
		Fingerprint: fp,
		Proxy:       proxy,
		Jar:         jar,
		HTTP: &http.Client{
			Transport: proxychain.WithDirectFallback(transport, explicitProxy),
			Jar:       jar,
			Timeout:   60 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 6 {
					return http.ErrUseLastResponse
				}
				return nil
			},
		},
	}
}

// postForm 发送 multipart/form-data。
func (c *WebClient) postForm(ctx context.Context, path string, fields map[string]string) (int, []byte, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return 0, nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return 0, nil, err
	}
	return c.do(ctx, http.MethodPost, path, mw.FormDataContentType(), &buf, 3)
}

// postJSON 发送 JSON。
func (c *WebClient) postJSON(ctx context.Context, path string, body any) (int, []byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	return c.do(ctx, http.MethodPost, path, "application/json", bytes.NewReader(raw), 3)
}

// sleepCtx 等待 d，期间 ctx 到期则立刻返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// do 发送请求（5xx / 网络错误重试）。
func (c *WebClient) do(ctx context.Context, method, path, contentType string, body io.Reader, attempts int) (int, []byte, error) {
	if attempts <= 0 {
		attempts = 1
	}
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = io.ReadAll(body)
		if err != nil {
			return 0, nil, err
		}
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		url := path
		if !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
			url = c.Origin + path
		}
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(bodyBytes))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("Origin", c.Site)
		req.Header.Set("Referer", c.Site+"/chat")
		req.Header.Set("x-deepai-language", c.Lang)
		req.Header.Set("Cache-Control", "no-cache")
		req.Header.Set("Pragma", "no-cache")
		req.Header.Set("Sec-Fetch-Dest", "empty")
		req.Header.Set("Sec-Fetch-Mode", "cors")
		req.Header.Set("Sec-Fetch-Site", "same-site")
		req.Header.Set("Priority", "u=1, i")
		if c.Fingerprint.AcceptLanguage != "" {
			req.Header.Set("Accept-Language", c.Fingerprint.AcceptLanguage)
		}
		if c.Fingerprint.SecCHUA != "" {
			req.Header.Set("sec-ch-ua", c.Fingerprint.SecCHUA)
			mobile := "?0"
			if c.Fingerprint.Mobile {
				mobile = "?1"
			}
			req.Header.Set("sec-ch-ua-mobile", mobile)
			if c.Fingerprint.Platform != "" {
				req.Header.Set("sec-ch-ua-platform", c.Fingerprint.Platform)
			}
		}
		if c.UA != "" {
			req.Header.Set("User-Agent", c.UA)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			if attempt < attempts-1 {
				// 退避必须可被 ctx 打断：否则 ctx 已经超时了还要把
				// 700ms/1400ms 睡完，注册 POST 的预算形同虚设。
				if !sleepCtx(ctx, time.Duration(attempt+1)*700*time.Millisecond) {
					return 0, nil, fmt.Errorf("%s %s: %w", method, path, ctx.Err())
				}
				continue
			}
			return 0, nil, fmt.Errorf("%s %s: %w", method, path, err)
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode >= 500 && attempt < attempts-1 {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			if !sleepCtx(ctx, time.Duration(attempt+1)*700*time.Millisecond) {
				// ctx 到期：把已拿到的状态码交回调用方，比丢掉更有用。
				return resp.StatusCode, raw, nil
			}
			continue
		}
		return resp.StatusCode, raw, nil
	}
	return 0, nil, lastErr
}

// verifyLinkAllowed 只放行「本客户端 Origin 下 /verify_email 路径」的链接。
//
// 历史上这里硬编码 api.deepai.org；改成跟随 c.Origin 后，指向自建/测试后端的
// 客户端也能验证，同时仍然挡住被当成开放代请求器的情况（路径与域名双重限制）。
func (c *WebClient) verifyLinkAllowed(link string) bool {
	u, err := url.Parse(link)
	if err != nil || u.Host == "" {
		return false
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return false
	}
	if !strings.HasPrefix(u.Path, "/verify_email") {
		return false
	}
	allowed := "api.deepai.org"
	if c.Origin != "" {
		if o, err := url.Parse(c.Origin); err == nil && o.Host != "" {
			allowed = o.Host
		}
	}
	return strings.EqualFold(u.Host, allowed)
}

// UserExists 查询邮箱是否已注册。
func (c *WebClient) UserExists(ctx context.Context, email string) (bool, error) {
	st, raw, err := c.postForm(ctx, "/get_user_login_type", map[string]string{"email": email})
	if err != nil {
		return false, err
	}
	if st != http.StatusOK {
		return false, &WebError{Op: "查询邮箱", Status: st, Message: apiMessage(raw)}
	}
	var out struct {
		UserExists        bool     `json:"user_exists"`
		LoginWithPassword bool     `json:"login_with_password"`
		SocialAccounts    []string `json:"login_with_social_accounts"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return false, fmt.Errorf("登录类型响应解析失败: %w", err)
	}
	return out.UserExists, nil
}

// SaveSignupData 保存注册来源（对齐网页端行为，失败不阻断）。
func (c *WebClient) SaveSignupData(ctx context.Context) error {
	body := map[string]any{
		"referrer":         "[]",
		"signup_page":      "/chat",
		"desired_username": nil,
		"language":         c.Lang,
	}
	st, raw, err := c.postJSON(ctx, "/save_custom_signup_data", body)
	if err != nil {
		return err
	}
	if st/100 != 2 {
		return &WebError{Op: "保存注册来源", Status: st, Message: apiMessage(raw)}
	}
	return nil
}

// 注册 POST 的总时间预算。
//
// 上游偶发 60s 级的 504（Gateway Timeout）：原来 3 次内部重试 × 60s ≈ 190s
// 才判死，一个账号就能把整批墙钟拖长两分钟。压到 45s 后快速失败，
// 由 pipeline 用「登录兜底」判断账号是否其实已经建成。
var registerPostBudget = 45 * time.Second

// Register 注册新账号，返回会话 key。
func (c *WebClient) Register(ctx context.Context, email, password string) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, registerPostBudget)
	defer cancel()
	st, raw, err := c.postForm(rctx, "/daily-time-sync/registration/", map[string]string{
		"email":     email,
		"username":  email,
		"password1": password,
		"password2": password,
	})
	if err != nil {
		return "", err
	}
	if st/100 != 2 {
		return "", &WebError{Op: "注册", Status: st, Message: apiMessage(raw)}
	}
	var out struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(raw, &out)
	return out.Key, nil
}

// Login 用邮箱 + 密码登录，返回会话 key。
func (c *WebClient) Login(ctx context.Context, email, password string) (string, error) {
	st, raw, err := c.postForm(ctx, "/daily-time-sync/login/", map[string]string{
		"username": email,
		"password": password,
	})
	if err != nil {
		return "", err
	}
	if st/100 != 2 {
		return "", &WebError{Op: "登录", Status: st, Message: apiMessage(raw)}
	}
	var out struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(raw, &out)
	return out.Key, nil
}

// FetchUser 读取当前会话的用户信息（含 API Key）。
func (c *WebClient) FetchUser(ctx context.Context) (*WebUser, error) {
	st, raw, err := c.do(ctx, http.MethodGet, "/daily-time-sync/user/", "", nil, 2)
	if err != nil {
		return nil, err
	}
	if st/100 != 2 {
		return nil, &WebError{Op: "读取会话", Status: st, Message: apiMessage(raw)}
	}
	var out WebUser
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("会话响应解析失败: %w", err)
	}
	return &out, nil
}

// VerifyEmail 访问验证链接。
func (c *WebClient) VerifyEmail(ctx context.Context, link string) error {
	link = strings.TrimSpace(link)
	if !c.verifyLinkAllowed(link) {
		return errors.New("验证链接域名不合法")
	}
	// 单次尝试：重试与退避统一由 verifyEmailThrottled 负责（限并发 + 长退避）。
	st, raw, err := c.do(ctx, http.MethodGet, link, "", nil, 1)
	if err != nil {
		return err
	}
	if st >= 400 {
		return &WebError{Op: "验证邮箱", Status: st, Message: apiMessage(raw)}
	}
	return nil
}

// Logout 注销当前网页会话（best-effort）。
func (c *WebClient) Logout(ctx context.Context) error {
	st, raw, err := c.do(ctx, http.MethodPost, "/daily-time-sync/logout/", "", nil, 1)
	if err != nil {
		return err
	}
	if st >= 400 {
		return &WebError{Op: "退出登录", Status: st, Message: apiMessage(raw)}
	}
	return nil
}

// Session 返回会话 Cookie 中的 sessionid / csrftoken。
func (c *WebClient) Session() (sessionID, csrf string) {
	if c.Jar == nil {
		return "", ""
	}
	u := mustURL(c.Origin)
	if u == nil {
		return "", ""
	}
	for _, ck := range c.Jar.Cookies(u) {
		switch ck.Name {
		case "sessionid":
			sessionID = ck.Value
		case "csrftoken":
			csrf = ck.Value
		}
	}
	return sessionID, csrf
}

// apiMessage 从错误响应里提取可读消息。
func apiMessage(raw []byte) string {
	obj := map[string]any{}
	if json.Unmarshal(raw, &obj) == nil {
		for _, key := range []string{"message", "non_field_errors", "detail", "error"} {
			if v, ok := obj[key]; ok {
				if s := firstMessage(v); s != "" {
					return s
				}
			}
		}
		for _, v := range obj {
			if s := firstMessage(v); s != "" {
				return s
			}
		}
	}
	return truncate(string(raw), 300)
}

func firstMessage(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case []any:
		for _, item := range x {
			if s := firstMessage(item); s != "" {
				return s
			}
		}
	case map[string]any:
		for _, item := range x {
			if s := firstMessage(item); s != "" {
				return s
			}
		}
	}
	return ""
}

func mustURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	return u
}
