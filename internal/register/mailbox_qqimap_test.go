package register

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/backend/memory"
	imapserver "github.com/emersion/go-imap/server"
)

type countedIMAPBackend struct {
	backend.Backend
	logins atomic.Int32
}

func (b *countedIMAPBackend) Login(info *imap.ConnInfo, user, pass string) (backend.User, error) {
	b.logins.Add(1)
	return b.Backend.Login(info, user, pass)
}

type imapTrace struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *imapTrace) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *imapTrace) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// Exercise a real TLS/IMAP server, not a mock of the provider's own methods.
func newIMAPFixture(t *testing.T, messages []*memory.Message) (*qqIMAPProvider, *countedIMAPBackend, *imapTrace) {
	t.Helper()
	certServer := httptest.NewTLSServer(nil)
	serverTLS := certServer.TLS.Clone()
	roots := x509.NewCertPool()
	roots.AddCert(certServer.Certificate())
	certServer.Close()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	mem := memory.New()
	u, err := mem.Login(nil, "username", "password")
	if err != nil {
		t.Fatal(err)
	}
	box, err := u.GetMailbox("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	box.(*memory.Mailbox).Messages = messages
	be := &countedIMAPBackend{Backend: mem}
	trace := new(imapTrace)
	srv := imapserver.New(be)
	srv.Debug = trace
	srv.ErrorLog = log.New(io.Discard, "", 0)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close(); <-done })
	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	p, err := newQQIMAPProvider(&Config{QQIMAPHost: host, QQIMAPPort: port, QQIMAPUser: "username", QQIMAPAuthCode: "password"}, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	p.tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	return p, be, trace
}

func imapTestMessage(uid uint32, received time.Time, to, extraHeaders, body string) *memory.Message {
	raw := "From: DeepAI <noreply@deepai.org>\r\nTo: " + to + "\r\nSubject: Verify DeepAI email\r\n" +
		extraHeaders + "\r\n" + body
	return &memory.Message{Uid: uid, Date: received, Size: uint32(len(raw)), Body: []byte(raw)}
}

func TestQQIMAPForwardedMailIsolationAndReadOnly(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	const firstLink = "https://api.deepai.org/verify_email?key=aaaa-1111"
	const secondLink = "https://api.deepai.org/verify_email?key=bbbb-2222"
	messages := []*memory.Message{
		imapTestMessage(1, now.Add(-time.Hour), "first@example.com", "Content-Type: text/plain\r\n", "https://api.deepai.org/verify_email?key=dead-0000"),
		imapTestMessage(2, now, "notfirst@example.com", "Content-Type: text/plain\r\n", "https://api.deepai.org/verify_email?key=dead-3333"),
		imapTestMessage(3, now, "second@example.com", "Content-Type: text/plain\r\n", secondLink),
		imapTestMessage(4, now, "123456@qq.com", "X-Original-To: first@example.com\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n", base64.StdEncoding.EncodeToString([]byte("<a href=\""+firstLink+"\">Verify</a>"))),
	}
	p, be, trace := newIMAPFixture(t, messages)
	if err := p.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for email, want := range map[string]string{"first@example.com": firstLink, "second@example.com": secondLink} {
		wg.Add(1)
		go func(email, want string) {
			defer wg.Done()
			mb, ok := p.Inbox(email)
			if !ok {
				t.Errorf("Inbox(%s) rejected", email)
				return
			}
			link, err := mb.WaitForVerification(context.Background(), now.Add(-time.Minute), time.Second, time.Millisecond)
			if err != nil || link != want {
				t.Errorf("%s: link=%q error=%v, want %q", email, link, err, want)
			}
		}(email, want)
	}
	wg.Wait()
	if got := be.logins.Load(); got != 2 {
		t.Errorf("wanted one health login and one shared scan, got %d", got)
	}
	for _, msg := range messages {
		if len(msg.Flags) != 0 {
			t.Errorf("message %d flags changed: %v", msg.Uid, msg.Flags)
		}
	}
	wire := strings.ToUpper(trace.String())
	if !strings.Contains(wire, "EXAMINE INBOX") || !strings.Contains(wire, "BODY.PEEK[]") {
		t.Errorf("expected read-only EXAMINE and BODY.PEEK in IMAP commands")
	}
	if _, ok := p.Inbox("first@other.example"); ok {
		t.Error("accepted another domain")
	}
	mb, err := p.Create(context.Background())
	if err != nil || !mb.Fresh() || !strings.HasSuffix(mb.Email(), "@example.com") {
		t.Fatalf("generated address: %v %v", mb, err)
	}
}

func TestQQIMAPTLSAndLoginErrors(t *testing.T) {
	p, _, _ := newIMAPFixture(t, nil)
	trustedTLS := p.tlsConfig
	p.tlsConfig = nil
	if err := p.Health(context.Background()); err == nil {
		t.Fatal("trusted an unknown TLS certificate")
	}
	p.tlsConfig = trustedTLS
	p.authCode = "do-not-echo-this-password"
	if err := p.Health(context.Background()); err == nil || strings.Contains(err.Error(), p.authCode) {
		t.Fatalf("expected sanitized authentication error, got %v", err)
	}
}

func TestQQIMAPCachedMailRespectsSinceAndCancellation(t *testing.T) {
	now := time.Now()
	p := &qqIMAPProvider{gate: make(chan struct{}, 1), cachedAt: now, cachedSince: now.Add(-time.Hour), cached: []Mail{{
		To: "first@example.com", ReceivedAt: now.Add(-time.Minute).UnixMilli(), Body: "https://api.deepai.org/verify_email?key=aaaa-1111",
	}}}
	mb := &qqIMAPInbox{provider: p, email: "first@example.com"}
	if link, err := mb.WaitForVerification(context.Background(), now, 25*time.Millisecond, time.Millisecond); link != "" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reused stale cached verification: %q %v", link, err)
	}
	p.gate <- struct{}{} // Cancellation must also interrupt waiting for another scanner.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.list(ctx, now); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled scan: %v", err)
	}
}

func TestReadIMAPMailQuotedPrintableAndRecipientBoundaries(t *testing.T) {
	raw := "To: User <first@example.com>\r\nSubject: =?UTF-8?B?RGVlcEFJIHZlcmlmeQ==?=\r\n" +
		"Content-Type: multipart/alternative; boundary=parts\r\n\r\n--parts\r\n" +
		"Content-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"<a href=3D\"https://api.deepai.org/verify_=\r\nemail&#63;key=3Daaaa-1111\">Verify</a>\r\n--parts--\r\n"
	m, err := readIMAPMail(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got := extractVerifyLink(m); got != "https://api.deepai.org/verify_email?key=aaaa-1111" {
		t.Fatalf("MIME link = %q", got)
	}
	if !mailForRecipient(m.To, "FIRST@example.com") {
		t.Error("failed matching exact recipient")
	}
	for _, wrong := range []string{"notfirst@example.com", "first+extra@example.com", "x!first@example.com", "first@example.com.evil"} {
		if mailForRecipient(wrong, "first@example.com") {
			t.Errorf("matched wrong recipient %q", wrong)
		}
	}
}

func TestQQIMAPConfigValidation(t *testing.T) {
	cfg := &Config{DefaultDomain: "example.com", QQIMAPUser: "123@qq.com", QQIMAPAuthCode: "auth-code"}
	if cfg.AutoBackend() != "qqimap" || len(cfg.Validate("qqimap")) != 0 {
		t.Fatalf("QQ IMAP not selected: %v", cfg.Validate("qqimap"))
	}
	p, err := newQQIMAPProvider(cfg, cfg.DefaultDomain)
	if err != nil || p.host != "imap.qq.com" || p.port != 993 || p.folder != "INBOX" {
		t.Fatalf("bad defaults: %v %v", p, err)
	}
	cfg.QQIMAPPort = -1
	if _, err := newQQIMAPProvider(cfg, cfg.DefaultDomain); err == nil {
		t.Error("accepted invalid port")
	}
	for _, domain := range []string{"", "example.com/path", "other@example.com", "example.com\r\nInjected: yes"} {
		if _, err := newQQIMAPProvider(cfg, domain); err == nil {
			t.Errorf("accepted invalid domain %q", domain)
		}
	}
	cfg.QQIMAPPort = 993
	if _, err := NewProvider(cfg, "qqimap", cfg.DefaultDomain, "other@wrong.example"); err == nil {
		t.Error("accepted an explicit email outside the forwarded domain")
	}
	cfg.QQIMAPUser, cfg.QQIMAPAuthCode = "", ""
	if len(cfg.Validate("qqimap")) != 2 {
		t.Errorf("missing credentials not reported: %v", cfg.Validate("qqimap"))
	}
}

func TestCheckQQIMAPProviderDoesNotUsePlaceholderMailbox(t *testing.T) {
	p, _, _ := newIMAPFixture(t, nil)
	cfg := &Config{
		DefaultDomain: "my-owned-domain.test", MailBackend: "qqimap",
		QQIMAPHost: p.host, QQIMAPPort: p.port,
		QQIMAPUser: "username", QQIMAPAuthCode: "password",
	}
	// The fixture uses a private CA, so the public health-check path must reach
	// TLS verification and reject the certificate, rather than reject a made-up
	// example.com mailbox before any connection is attempted.
	for _, backend := range []string{"qqimap", ""} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := CheckProvider(ctx, cfg, backend, cfg.DefaultDomain)
		cancel()
		if err == nil || !strings.Contains(err.Error(), "IMAP TLS 连接失败") {
			t.Fatalf("backend=%q: health check did not reach IMAP TLS: %v", backend, err)
		}
	}
	if _, err := NewProvider(cfg, "qqimap", cfg.DefaultDomain, "probe@example.com"); err == nil {
		t.Fatal("registration must still reject a real email from another domain")
	}
}
