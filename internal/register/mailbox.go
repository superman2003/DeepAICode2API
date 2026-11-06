package register

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"deepaicode2api/internal/deepai"
)

// adaptivePollFloor 是自适应轮询的起始间隔。
//
// 收信等待的实测中位数是 14.8s；把首轮间隔从 1s 降到 300ms，
// 平均能省下约 0.35s/号，而稳态仍是配置的 PollInterval。
const adaptivePollFloor = 300 * time.Millisecond

// ErrNoInbox 表示当前邮箱后端没有收件箱（无法自动点验证链接）。
var ErrNoInbox = errors.New("邮箱后端没有收件箱")

// verifyLinkRe 匹配 DeepAI 的邮箱验证链接。
var verifyLinkRe = regexp.MustCompile(`https?://api\.deepai\.org/verify_email\?key=[0-9a-fA-F-]+`)

// Mail 是一封收件箱邮件。
type Mail struct {
	ID         string
	To         string
	From       string
	Subject    string
	Body       string
	ReceivedAt int64 // 毫秒
}

// Mailbox 是可收信的邮箱。
type Mailbox interface {
	Email() string
	Provider() string
	// Fresh 报告这个地址是否是本次新建的。
	//
	// 新建地址必然未注册，"邮箱是否已存在"的预检（一次网络往返，实测 ~3.3s）
	// 可以直接跳过；补跑 / 用户指定的地址则必须预检。
	Fresh() bool
	// WaitForVerification 轮询等待验证邮件并返回验证链接；无收件箱时返回 ErrNoInbox。
	WaitForVerification(ctx context.Context, since time.Time, timeout, interval time.Duration) (string, error)
}

// Provider 是邮箱后端工厂。
type Provider interface {
	Name() string
	Create(ctx context.Context) (Mailbox, error)
}

// InboxProvider 允许为已有地址打开收件箱（不新建邮箱）。
type InboxProvider interface {
	Inbox(email string) (Mailbox, bool)
}

// HealthProvider 允许后端做只读健康检查（doctor 用）。
type HealthProvider interface {
	Health(ctx context.Context) error
}

// NewProvider 按名字构造邮箱后端。
//
// backend 为空时依次选择已配置的 cf、qqimap、tempmail_lol、static、none。
func NewProvider(cfg *Config, backend, domain, email string) (Provider, error) {
	if cfg == nil {
		cfg = LoadConfig()
	}
	name := strings.ToLower(strings.TrimSpace(backend))
	if name == "" {
		name = cfg.AutoBackend()
	}
	switch name {
	case "cf":
		base := strings.TrimRight(cfg.TempMailBase, "/")
		if base == "" {
			return nil, errors.New("TEMPMAIL_BASE 未配置")
		}
		if domain == "" {
			domain = firstNonEmpty(cfg.TempMailDomain, cfg.DefaultDomain)
		}
		return &cfProvider{base: base, adminKey: cfg.TempMailAdminKey, domain: domain, ua: cfg.UserAgent}, nil
	case "qqimap":
		if domain == "" {
			domain = cfg.DefaultDomain
		}
		provider, err := newQQIMAPProvider(cfg, domain)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(email) != "" {
			if _, ok := provider.Inbox(email); !ok {
				return nil, errors.New("QQ IMAP 指定邮箱必须属于配置的注册域名")
			}
		}
		return provider, nil
	case "tempmail_lol":
		provider, err := newTempMailLOLProvider(cfg, domain)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(email) != "" {
			if _, ok := provider.Inbox(email); !ok {
				return nil, errors.New("TempMail.lol 指定邮箱没有已保存的收信令牌，请留空自动创建邮箱")
			}
		}
		return provider, nil
	case "static":
		if domain == "" {
			domain = cfg.DefaultDomain
		}
		if domain == "" {
			return nil, errors.New("static 邮箱后端缺少域名（DEEPAI_REGISTER_DOMAIN）")
		}
		return &staticProvider{domain: domain, prefix: cfg.DefaultPrefix}, nil
	case "none":
		if strings.TrimSpace(email) == "" {
			return nil, errors.New("mail-backend=none 时必须显式提供邮箱")
		}
		return &noneProvider{email: strings.TrimSpace(email)}, nil
	default:
		return nil, fmt.Errorf("未知邮箱后端 %q（可选 cf / qqimap / tempmail_lol / static / none）", backend)
	}
}

// --- CF Temp Email Worker ---

type cfProvider struct {
	base     string
	adminKey string
	domain   string
	ua       string
}

func (p *cfProvider) Name() string { return "cf" }

func (p *cfProvider) client() *http.Client {
	return &http.Client{Timeout: 35 * time.Second}
}

// request 发送请求并处理 5xx 重试 / 4xx 立刻失败。
func (p *cfProvider) request(ctx context.Context, method, path string, body any, admin bool) ([]byte, error) {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	var last error
	for attempt := 0; attempt < 4; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, p.base+path, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		if p.ua != "" {
			req.Header.Set("User-Agent", p.ua)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if admin {
			if p.adminKey == "" {
				return nil, errors.New("TEMPMAIL_ADMIN_KEY 未配置")
			}
			req.Header.Set("Authorization", "Bearer "+p.adminKey)
		}
		resp, err := p.client().Do(req)
		if err != nil {
			last = err
			if attempt < 3 {
				time.Sleep(time.Duration(attempt+1) * 800 * time.Millisecond)
				continue
			}
			return nil, fmt.Errorf("邮箱服务网络失败 %s: %w", path, err)
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			last = readErr
			continue
		}
		if resp.StatusCode >= 500 {
			last = fmt.Errorf("邮箱服务 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
			if attempt < 3 {
				time.Sleep(time.Duration(attempt+1) * 800 * time.Millisecond)
				continue
			}
			return nil, fmt.Errorf("邮箱服务持续 5xx: %w", last)
		}
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("邮箱服务 HTTP %d %s: %s", resp.StatusCode, path, truncate(string(raw), 240))
		}
		return raw, nil
	}
	return nil, fmt.Errorf("邮箱服务重试耗尽 %s: %w", path, last)
}

// Health 检查邮箱服务是否可用（只读）。
func (p *cfProvider) Health(ctx context.Context) error {
	raw, err := p.request(ctx, http.MethodGet, "/health", nil, false)
	if err != nil {
		return err
	}
	if !strings.Contains(string(raw), `"ok"`) {
		return fmt.Errorf("邮箱服务健康检查异常: %s", truncate(string(raw), 200))
	}
	return nil
}

func (p *cfProvider) Create(ctx context.Context) (Mailbox, error) {
	body := map[string]any{}
	if p.domain != "" {
		body["domain"] = p.domain
	}
	raw, err := p.request(ctx, http.MethodPost, "/api/mailboxes", body, true)
	if err != nil {
		return nil, err
	}
	var out struct {
		Emails []string `json:"emails"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("建邮箱响应解析失败: %w", err)
	}
	if len(out.Emails) == 0 || strings.TrimSpace(out.Emails[0]) == "" {
		return nil, fmt.Errorf("建邮箱未返回地址: %s", truncate(string(raw), 240))
	}
	return &cfInbox{provider: p, email: strings.TrimSpace(out.Emails[0]), fresh: true}, nil
}

func (p *cfProvider) Inbox(email string) (Mailbox, bool) {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, false
	}
	if p.domain != "" {
		_, domain, ok := strings.Cut(email, "@")
		if !ok || !strings.EqualFold(domain, p.domain) {
			return nil, false
		}
	}
	return &cfInbox{provider: p, email: email}, true
}

type cfInbox struct {
	provider *cfProvider
	email    string
	fresh    bool
}

func (m *cfInbox) Email() string    { return m.email }
func (m *cfInbox) Provider() string { return "cf" }
func (m *cfInbox) Fresh() bool      { return m.fresh }

func (m *cfInbox) list(ctx context.Context) ([]Mail, error) {
	path := "/api/inbox?" + url.Values{"email": {m.email}}.Encode()
	raw, err := m.provider.request(ctx, http.MethodGet, path, nil, false)
	if err != nil {
		return nil, err
	}
	var body struct {
		Messages []struct {
			ID          any    `json:"id"`
			ToAddress   string `json:"to_address"`
			To          string `json:"to"`
			FromAddress string `json:"from_address"`
			From        string `json:"from"`
			Subject     string `json:"subject"`
			Body        string `json:"body"`
			Text        string `json:"text"`
			RawText     string `json:"raw_text"`
			ReceivedAt  int64  `json:"received_at"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("收件箱响应解析失败: %w", err)
	}
	out := make([]Mail, 0, len(body.Messages))
	for _, msg := range body.Messages {
		m := Mail{
			To:         firstNonEmpty(msg.ToAddress, msg.To),
			From:       firstNonEmpty(msg.FromAddress, msg.From),
			Subject:    msg.Subject,
			Body:       firstNonEmpty(msg.Body, msg.Text, msg.RawText),
			ReceivedAt: msg.ReceivedAt,
		}
		m.ID = fmt.Sprintf("%v", msg.ID)
		out = append(out, m)
	}
	return out, nil
}

func (m *cfInbox) WaitForVerification(ctx context.Context, since time.Time, timeout, interval time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = DefaultVerifyWait
	}
	if interval <= 0 {
		interval = DefaultPollEvery
	}
	deadline := time.Now().Add(timeout)
	var lastErr error

	// 自适应轮询：验证邮件通常在注册后几秒内到达，固定 1s 间隔会白白
	// 多等半秒左右；改成从 300ms 起、每次 ×1.5 退避到配置间隔，
	// 既降低平均等待，又不会对邮箱服务造成成倍的请求量。
	wait := interval
	if wait > adaptivePollFloor {
		wait = adaptivePollFloor
	}

	for {
		mails, err := m.list(ctx)
		if err != nil {
			lastErr = err
		} else {
			for _, mail := range mails {
				if since.UnixMilli() > 0 && mail.ReceivedAt > 0 && mail.ReceivedAt < since.UnixMilli() {
					continue
				}
				link := extractVerifyLink(mail)
				if link != "" {
					return link, nil
				}
			}
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return "", fmt.Errorf("等待验证邮件超时（%s）：%v", timeout, lastErr)
			}
			return "", fmt.Errorf("等待验证邮件超时（%s）", timeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(wait):
		}
		if wait < interval {
			wait = wait * 3 / 2
			if wait > interval {
				wait = interval
			}
		}
	}
}

// extractVerifyLink 从主题 / 正文中提取验证链接。
func extractVerifyLink(m Mail) string {
	text := m.Subject + "\n" + m.Body
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "verif") && !strings.Contains(lower, "verify_email") {
		return ""
	}
	return verifyLinkRe.FindString(text)
}

// --- 静态域名后端（无收件箱） ---

type staticProvider struct {
	domain string
	prefix string
}

func (p *staticProvider) Name() string { return "static" }

func (p *staticProvider) Create(_ context.Context) (Mailbox, error) {
	prefix := strings.TrimSpace(p.prefix)
	if prefix == "" {
		prefix = "dai"
	}
	local := prefix + deepai.RandomHex(5)
	return &noInboxMailbox{email: local + "@" + p.domain, provider: "static", fresh: true}, nil
}

// --- 固定邮箱后端（无收件箱） ---

type noneProvider struct{ email string }

func (p *noneProvider) Name() string { return "none" }

func (p *noneProvider) Create(_ context.Context) (Mailbox, error) {
	return &noInboxMailbox{email: p.email, provider: "none"}, nil
}

// --- 无收件箱邮箱 ---

type noInboxMailbox struct {
	email    string
	provider string
	fresh    bool
}

func (m *noInboxMailbox) Email() string    { return m.email }
func (m *noInboxMailbox) Provider() string { return m.provider }
func (m *noInboxMailbox) Fresh() bool      { return m.fresh }
func (m *noInboxMailbox) WaitForVerification(context.Context, time.Time, time.Duration, time.Duration) (string, error) {
	return "", ErrNoInbox
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// CheckProvider 对邮箱后端做只读健康检查；无健康检查接口时直接通过。
func CheckProvider(ctx context.Context, cfg *Config, backend, domain string) error {
	if cfg == nil {
		cfg = LoadConfig()
	}
	name := strings.ToLower(strings.TrimSpace(backend))
	if name == "" {
		name = cfg.AutoBackend()
	}
	// A connection check has no registration recipient. Only the fixed-email
	// backend needs a placeholder to construct its no-inbox provider.
	email := ""
	if name == "none" {
		email = "probe@example.com"
	}
	p, err := NewProvider(cfg, name, domain, email)
	if err != nil {
		return err
	}
	if hp, ok := p.(HealthProvider); ok {
		return hp.Health(ctx)
	}
	return nil
}
