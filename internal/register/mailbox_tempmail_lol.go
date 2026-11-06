package register

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"deepaicode2api/internal/deepai"
)

var errTempMailLOLExpired = errors.New("TempMail.lol 临时邮箱已过期，请创建新邮箱")

type tempMailLOLProvider struct {
	base, key, domain, prefix, tokenDir string
	http                                *http.Client
	cooldown                            *tempMailLOLReadGate
}

func newTempMailLOLProvider(cfg *Config, domain string) (*tempMailLOLProvider, error) {
	base := strings.TrimRight(firstNonEmpty(cfg.TempMailLOLBase, "https://api.tempmail.lol/v2"), "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("TEMPMAIL_LOL_BASE 必须是有效的 HTTP/HTTPS API 地址")
	}
	return &tempMailLOLProvider{
		base: base, key: strings.TrimSpace(cfg.TempMailLOLKey), domain: strings.TrimSpace(domain),
		prefix:   firstNonEmpty(cfg.DefaultPrefix, "dai"),
		tokenDir: firstNonEmpty(cfg.TempMailLOLTokenDir, filepath.Join("exports", "tempmail-lol-inboxes")),
		http:     &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		cooldown: tempMailLOLGate("key\x00"+base+"\x00"+strings.TrimSpace(cfg.TempMailLOLKey), 0),
	}, nil
}

func (p *tempMailLOLProvider) Name() string { return "tempmail_lol" }

type tempMailLOLAPIError struct {
	status     int
	retryAfter time.Duration
	message    string
}

func (e *tempMailLOLAPIError) Error() string {
	return fmt.Sprintf("TempMail.lol HTTP %d: %s", e.status, e.message)
}

func (p *tempMailLOLProvider) redact(message string, token string) string {
	for _, secret := range []string{p.key, token, url.QueryEscape(token)} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "***")
		}
	}
	return message
}

func (p *tempMailLOLProvider) request(ctx context.Context, method, path string, body any, token string) ([]byte, error) {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base+path, payload)
	if err != nil {
		return nil, errors.New("TempMail.lol 请求地址无效")
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if p.key != "" {
		req.Header.Set("Authorization", "Bearer "+p.key)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("TempMail.lol 网络请求失败: %s", p.redact(err.Error(), token))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, errors.New("读取 TempMail.lol 响应失败")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if token != "" && (resp.StatusCode == 404 || resp.StatusCode == 410) {
			return nil, errTempMailLOLExpired
		}
		message := p.redact(strings.TrimSpace(string(raw)), token)
		if len(message) > 240 {
			message = message[:240]
		}
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			message = "API Key 无效、权限不足或服务拒绝访问"
		}
		retry := time.Duration(0)
		if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
			retry = time.Duration(seconds) * time.Second
			if retry > 2*time.Minute {
				retry = 2 * time.Minute
			}
		}
		if resp.StatusCode == http.StatusTooManyRequests && retry < tempMailLOLMinPollInterval {
			retry = 15 * time.Second // Some 429 responses omit Retry-After.
		}
		return nil, &tempMailLOLAPIError{status: resp.StatusCode, retryAfter: retry, message: message}
	}
	return raw, nil
}

type tempMailLOLToken struct {
	Address string `json:"address"`
	Token   string `json:"token"`
}

func (p *tempMailLOLProvider) tokenPath(email string) string {
	hash := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return filepath.Join(p.tokenDir, hex.EncodeToString(hash[:])+".json")
}

func (p *tempMailLOLProvider) saveToken(inbox tempMailLOLToken) error {
	if err := os.MkdirAll(p.tokenDir, 0700); err != nil {
		return errors.New("无法创建 TempMail.lol 收信令牌目录")
	}
	raw, err := json.Marshal(inbox)
	if err != nil {
		return err
	}
	path := p.tokenPath(inbox.Address)
	temporary := path + "." + deepai.RandomHex(6) + ".tmp"
	if err := os.WriteFile(temporary, raw, 0600); err != nil {
		return errors.New("保存 TempMail.lol 收信令牌失败")
	}
	defer os.Remove(temporary)
	if err := os.Rename(temporary, path); err != nil {
		return errors.New("保存 TempMail.lol 收信令牌失败")
	}
	return nil
}

func (p *tempMailLOLProvider) Create(ctx context.Context) (Mailbox, error) {
	body := map[string]any{"prefix": p.prefix + deepai.RandomHex(6)}
	if p.domain != "" {
		body["domain"] = p.domain
	}
	raw, err := p.request(ctx, http.MethodPost, "/inbox/create", body, "")
	if err != nil {
		return nil, err
	}
	var inbox tempMailLOLToken
	if err := json.Unmarshal(raw, &inbox); err != nil {
		return nil, errors.New("TempMail.lol 创建邮箱响应格式错误")
	}
	inbox.Address, inbox.Token = strings.TrimSpace(inbox.Address), strings.TrimSpace(inbox.Token)
	address, err := mail.ParseAddress(inbox.Address)
	if err != nil || address.Address != inbox.Address || inbox.Token == "" {
		return nil, errors.New("TempMail.lol 未返回有效的邮箱地址和收信令牌")
	}
	if err := p.saveToken(inbox); err != nil {
		return nil, err
	}
	return &tempMailLOLInbox{provider: p, email: inbox.Address, token: inbox.Token, fresh: true}, nil
}

func (p *tempMailLOLProvider) Inbox(email string) (Mailbox, bool) {
	raw, err := os.ReadFile(p.tokenPath(email))
	if err != nil {
		return nil, false
	}
	var inbox tempMailLOLToken
	if json.Unmarshal(raw, &inbox) != nil || !strings.EqualFold(inbox.Address, strings.TrimSpace(email)) || inbox.Token == "" {
		return nil, false
	}
	return &tempMailLOLInbox{provider: p, email: inbox.Address, token: inbox.Token}, true
}

type tempMailLOLInbox struct {
	provider     *tempMailLOLProvider
	email, token string
	fresh        bool
}

func (m *tempMailLOLInbox) Email() string    { return m.email }
func (m *tempMailLOLInbox) Provider() string { return "tempmail_lol" }
func (m *tempMailLOLInbox) Fresh() bool      { return m.fresh }

func (m *tempMailLOLInbox) list(ctx context.Context) ([]Mail, error) {
	if err := m.provider.inboxGate(m.token).wait(ctx, m.provider.cooldown); err != nil {
		return nil, err
	}
	raw, err := m.provider.request(ctx, http.MethodGet, "/inbox?"+url.Values{"token": {m.token}}.Encode(), nil, m.token)
	if err != nil {
		var apiErr *tempMailLOLAPIError
		if errors.As(err, &apiErr) && apiErr.status == http.StatusTooManyRequests {
			m.provider.cooldown.pause(apiErr.retryAfter)
		}
		return nil, err
	}
	var response struct {
		Expired bool `json:"expired"`
		Emails  []struct {
			From    string          `json:"from"`
			To      string          `json:"to"`
			Subject string          `json:"subject"`
			Body    string          `json:"body"`
			HTML    string          `json:"html"`
			Date    json.RawMessage `json:"date"`
		} `json:"emails"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return nil, errors.New("TempMail.lol 收件箱响应格式错误")
	}
	if response.Expired {
		return nil, errTempMailLOLExpired
	}
	mails := make([]Mail, 0, len(response.Emails))
	for _, email := range response.Emails {
		date, _ := strconv.ParseFloat(strings.Trim(string(email.Date), `"`), 64)
		if date > 0 && date < 1e12 {
			date *= 1000
		}
		mails = append(mails, Mail{To: email.To, From: email.From, Subject: email.Subject,
			Body: html.UnescapeString(email.Body + "\n" + email.HTML), ReceivedAt: int64(date)})
	}
	sort.SliceStable(mails, func(i, j int) bool { return mails[i].ReceivedAt > mails[j].ReceivedAt })
	return mails, nil
}

func (m *tempMailLOLInbox) WaitForVerification(ctx context.Context, since time.Time, timeout, interval time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = DefaultVerifyWait
	}
	if interval < tempMailLOLMinPollInterval {
		interval = tempMailLOLMinPollInterval
	}
	if !m.fresh {
		since = time.Time{}
	} // A saved inbox may already contain its verification mail.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for {
		mails, err := m.list(ctx)
		wait := interval
		if err != nil {
			lastErr = err
			var apiErr *tempMailLOLAPIError
			if errors.Is(err, errTempMailLOLExpired) {
				return "", err
			}
			if errors.As(err, &apiErr) {
				if apiErr.status == 401 || apiErr.status == 403 {
					return "", err
				}
				if apiErr.retryAfter > wait {
					wait = apiErr.retryAfter
				}
			}
		} else {
			lastErr = nil
			for _, email := range mails {
				if !since.IsZero() && email.ReceivedAt > 0 && email.ReceivedAt < since.UnixMilli() {
					continue
				}
				if email.To != "" && !mailForRecipient(email.To, m.email) {
					continue
				}
				if link := extractVerifyLink(email); link != "" {
					return link, nil
				}
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			if lastErr != nil {
				return "", fmt.Errorf("等待 TempMail.lol 验证邮件结束: %w（%v）", ctx.Err(), lastErr)
			}
			return "", fmt.Errorf("等待 TempMail.lol 验证邮件结束: %w", ctx.Err())
		case <-timer.C:
		}
	}
}
