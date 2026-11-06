package register

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/mail"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"deepaicode2api/internal/deepai"
	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	_ "github.com/emersion/go-message/charset"
	mailreader "github.com/emersion/go-message/mail"
)

const (
	qqIMAPOperationTimeout = 15 * time.Second
	qqIMAPPollInterval     = 3 * time.Second
	qqIMAPMaxMessages      = 100
	qqIMAPMaxMessageBytes  = 1 << 20
)

// A provider is shared by all workers in a registration job. Serialize IMAP
// scans and share a short cache so concurrent domain addresses use one login.
type qqIMAPProvider struct {
	domain, prefix, host, user, authCode, folder string
	port                                         int
	tlsConfig                                    *tls.Config
	gate                                         chan struct{}
	cachedAt, cachedSince                        time.Time
	cached                                       []Mail
	cachedErr                                    error
}

func newQQIMAPProvider(cfg *Config, domain string) (*qqIMAPProvider, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	address, err := mail.ParseAddress("probe@" + domain)
	if err != nil || domain == "" || address.Address != "probe@"+domain || strings.ContainsAny(domain, " /\\:\t\r\n") {
		return nil, errors.New("QQ IMAP 后端需要有效的注册邮箱域名，例如 example.com")
	}
	if strings.TrimSpace(cfg.QQIMAPUser) == "" || strings.TrimSpace(cfg.QQIMAPAuthCode) == "" {
		return nil, errors.New("请填写 QQ 邮箱和 IMAP 授权码，或配置 QQ_IMAP_USER / QQ_IMAP_AUTH_CODE")
	}
	port := cfg.QQIMAPPort
	if port == 0 {
		port = 993
	}
	if port < 1 || port > 65535 {
		return nil, errors.New("QQ_IMAP_PORT 必须在 1-65535 之间")
	}
	prefix := firstNonEmpty(cfg.DefaultPrefix, "dai")
	if a, err := mail.ParseAddress(prefix + "123@" + domain); err != nil || a.Address != prefix+"123@"+domain {
		return nil, errors.New("DEEPAI_REGISTER_PREFIX 不是有效的邮箱前缀")
	}
	return &qqIMAPProvider{
		domain: domain, prefix: prefix,
		host: firstNonEmpty(cfg.QQIMAPHost, "imap.qq.com"), port: port,
		user:     strings.TrimSpace(cfg.QQIMAPUser),
		authCode: strings.Join(strings.Fields(cfg.QQIMAPAuthCode), ""),
		folder:   firstNonEmpty(cfg.QQIMAPFolder, "INBOX"), gate: make(chan struct{}, 1),
	}, nil
}

func (p *qqIMAPProvider) Name() string { return "qqimap" }

func (p *qqIMAPProvider) Create(ctx context.Context) (Mailbox, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &qqIMAPInbox{provider: p, email: p.prefix + deepai.RandomHex(8) + "@" + p.domain, fresh: true}, nil
}

func (p *qqIMAPProvider) Inbox(email string) (Mailbox, bool) {
	email = strings.TrimSpace(email)
	a, err := mail.ParseAddress(email)
	if err != nil || a.Address != email {
		return nil, false
	}
	_, domain, _ := strings.Cut(email, "@")
	if !strings.EqualFold(domain, p.domain) {
		return nil, false
	}
	return &qqIMAPInbox{provider: p, email: email}, true
}

func (p *qqIMAPProvider) connect(ctx context.Context) (*client.Client, func(), error) {
	operationCtx, cancel := context.WithTimeout(ctx, qqIMAPOperationTimeout)
	config := p.tlsConfig
	if config == nil {
		config = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	dialer := &tls.Dialer{Config: config}
	conn, err := dialer.DialContext(operationCtx, "tcp", net.JoinHostPort(p.host, strconv.Itoa(p.port)))
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("IMAP TLS 连接失败: %w", err)
	}
	stop := context.AfterFunc(operationCtx, func() { _ = conn.Close() })
	cleanup := func() { stop(); cancel(); _ = conn.Close() }
	c, err := client.New(conn)
	if err != nil {
		cleanup()
		return nil, nil, errors.New("读取 IMAP 服务响应失败")
	}
	c.Timeout = qqIMAPOperationTimeout
	c.ErrorLog = log.New(io.Discard, "", 0)
	if err := c.Login(p.user, p.authCode); err != nil {
		cleanup()
		// Never include server login errors: a server may echo credentials.
		return nil, nil, errors.New("QQ IMAP 登录失败，请确认已开启 IMAP 服务，并使用授权码")
	}
	if _, err := c.Select(p.folder, true); err != nil { // EXAMINE: read-only mailbox
		cleanup()
		return nil, nil, errors.New("无法只读打开 IMAP 收件文件夹，请检查 QQ_IMAP_FOLDER")
	}
	return c, cleanup, nil
}

func (p *qqIMAPProvider) Health(ctx context.Context) error {
	_, closeConn, err := p.connect(ctx)
	if err != nil {
		return err
	}
	closeConn()
	return nil
}

func (p *qqIMAPProvider) list(ctx context.Context, since time.Time) ([]Mail, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case p.gate <- struct{}{}:
		defer func() { <-p.gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !p.cachedAt.IsZero() && time.Since(p.cachedAt) < qqIMAPPollInterval && !since.Before(p.cachedSince) {
		return p.cached, p.cachedErr
	}
	mails, err := p.scan(ctx, since)
	p.cachedAt, p.cachedSince, p.cached, p.cachedErr = time.Now(), since, mails, err
	return mails, err
}

func (p *qqIMAPProvider) scan(ctx context.Context, since time.Time) ([]Mail, error) {
	c, closeConn, err := p.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer closeConn()
	criteria := imap.NewSearchCriteria()
	if !since.IsZero() {
		// SEARCH SINCE has day resolution and server-local timezone semantics.
		criteria.Since = since.Add(-24 * time.Hour)
	}
	from, subject := imap.NewSearchCriteria(), imap.NewSearchCriteria()
	from.Header.Set("From", "deepai")
	subject.Header.Set("Subject", "DeepAI")
	criteria.Or = [][2]*imap.SearchCriteria{{from, subject}}
	uids, err := c.UidSearch(criteria)
	if err != nil {
		return nil, errors.New("IMAP 搜索验证邮件失败")
	}
	if len(uids) == 0 {
		return nil, nil
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	if len(uids) > qqIMAPMaxMessages {
		uids = uids[len(uids)-qqIMAPMaxMessages:]
	}
	set := new(imap.SeqSet)
	set.AddNum(uids...)
	section := &imap.BodySectionName{Peek: true, Partial: []int{0, qqIMAPMaxMessageBytes}}
	messages := make(chan *imap.Message, len(uids))
	done := make(chan error, 1)
	go func() {
		done <- c.UidFetch(set, []imap.FetchItem{imap.FetchUid, imap.FetchInternalDate, section.FetchItem()}, messages)
	}()
	var mails []Mail
	for msg := range messages {
		if msg == nil || (!since.IsZero() && msg.InternalDate.Before(since)) {
			continue
		}
		body := msg.GetBody(section)
		if body == nil {
			continue
		}
		m, err := readIMAPMail(body)
		if err != nil {
			continue // Ignore unrelated or malformed messages; keep checking others.
		}
		m.ID, m.ReceivedAt = strconv.FormatUint(uint64(msg.Uid), 10), msg.InternalDate.UnixMilli()
		mails = append(mails, m)
	}
	if err := <-done; err != nil {
		return nil, errors.New("IMAP 读取验证邮件失败")
	}
	// Prefer the latest link when the service has sent multiple verifications.
	sort.Slice(mails, func(i, j int) bool { return mails[i].ReceivedAt > mails[j].ReceivedAt })
	return mails, nil
}

type qqIMAPInbox struct {
	provider *qqIMAPProvider
	email    string
	fresh    bool
}

func (m *qqIMAPInbox) Email() string    { return m.email }
func (m *qqIMAPInbox) Provider() string { return "qqimap" }
func (m *qqIMAPInbox) Fresh() bool      { return m.fresh }

func (m *qqIMAPInbox) WaitForVerification(ctx context.Context, since time.Time, timeout, interval time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = DefaultVerifyWait
	}
	if interval < qqIMAPPollInterval {
		interval = qqIMAPPollInterval
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for {
		mails, err := m.provider.list(ctx, since)
		if err != nil {
			lastErr = err
		} else {
			for _, mail := range mails {
				if !since.IsZero() && mail.ReceivedAt < since.UnixMilli() {
					continue
				}
				if !mailForRecipient(mail.To, m.email) {
					continue
				}
				if link := extractVerifyLink(mail); link != "" {
					return link, nil
				}
			}
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if lastErr != nil {
				return "", fmt.Errorf("等待 QQ IMAP 验证邮件结束: %w（%v）", ctx.Err(), lastErr)
			}
			return "", fmt.Errorf("等待 QQ IMAP 验证邮件结束: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

var recipientAddressRE = regexp.MustCompile("(?i)[a-z0-9.!#$%&'*+/=?^_`{|}~-]+@[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?")

func mailForRecipient(headers, recipient string) bool {
	for _, address := range recipientAddressRE.FindAllString(headers, -1) {
		if strings.EqualFold(address, recipient) {
			return true
		}
	}
	return false
}

func readIMAPMail(raw io.Reader) (Mail, error) {
	r, err := mailreader.CreateReader(io.LimitReader(raw, qqIMAPMaxMessageBytes))
	if err != nil {
		return Mail{}, err
	}
	defer r.Close()
	subject, _ := r.Header.Subject()
	m := Mail{Subject: subject, From: r.Header.Get("From")}
	// Forwarders normally preserve To, but may put the original recipient in
	// delivery headers. Never match on the body or the QQ destination alone.
	for _, key := range []string{"To", "Cc", "Delivered-To", "X-Original-To", "X-Envelope-To", "Envelope-To", "X-Original-Recipient", "Original-Recipient", "X-Forwarded-To", "Received"} {
		fields := r.Header.FieldsByKey(key)
		for fields.Next() {
			m.To += "\n" + fields.Value()
		}
	}
	var body strings.Builder
	for parts := 0; parts < 100 && body.Len() < qqIMAPMaxMessageBytes; parts++ {
		part, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Mail{}, err
		}
		if header, ok := part.Header.(*mailreader.InlineHeader); ok {
			mediaType, _, _ := header.ContentType()
			if mediaType == "text/plain" || mediaType == "text/html" || mediaType == "" {
				text, err := io.ReadAll(io.LimitReader(part.Body, int64(qqIMAPMaxMessageBytes-body.Len())))
				if err != nil {
					return Mail{}, err
				}
				body.Write(text)
				body.WriteByte('\n')
			}
		}
	}
	m.Body = html.UnescapeString(body.String())
	return m, nil
}
