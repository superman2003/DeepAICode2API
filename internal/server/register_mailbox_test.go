package server

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"deepaicode2api/internal/register"
	"deepaicode2api/internal/store"
)

func TestMailboxConfigRequestAppliesQQIMAPFields(t *testing.T) {
	var request struct{ mailboxConfigRequest }
	if err := json.Unmarshal([]byte(`{"mail_backend":"qqimap","domain":"example.com","qq_imap_user":"123@qq.com","qq_imap_auth_code":"test-secret","qq_imap_host":"imap.qq.com","qq_imap_port":993,"qq_imap_folder":"INBOX"}`), &request); err != nil {
		t.Fatal(err)
	}
	cfg := &register.Config{}
	request.mailboxConfigRequest.apply(cfg)
	if cfg.AutoBackend() != "qqimap" || len(cfg.Validate("qqimap")) != 0 || cfg.QQIMAPAuthCode != "test-secret" || cfg.QQIMAPPort != 993 {
		t.Fatal("QQ IMAP form fields did not reach mailbox config")
	}
	provider, err := register.NewProvider(cfg, cfg.AutoBackend(), cfg.DefaultDomain, "")
	if err != nil || provider.Name() != "qqimap" {
		t.Fatalf("provider: %v %v", provider, err)
	}
}

func TestTempMailLOLConfigDoesNotInheritQQDomain(t *testing.T) {
	var request struct{ mailboxConfigRequest }
	if err := json.Unmarshal([]byte(`{"mail_backend":"tempmail_lol","domain":"","tempmail_lol_api_key":"request-secret"}`), &request); err != nil {
		t.Fatal(err)
	}
	cfg := &register.Config{DefaultDomain: "old-qq-domain.example", TempMailLOLKey: "env-secret"}
	request.mailboxConfigRequest.apply(cfg)
	if cfg.DefaultDomain != "" || cfg.TempMailLOLKey != "request-secret" || cfg.AutoBackend() != "tempmail_lol" {
		t.Fatal("external provider inherited QQ domain or ignored request config")
	}
	provider, err := register.NewProvider(cfg, cfg.AutoBackend(), cfg.DefaultDomain, "")
	if err != nil || provider.Name() != "tempmail_lol" {
		t.Fatalf("external provider: %v %v", provider, err)
	}
}

func TestMailboxTestQQIMAPUsesConfiguredDomain(t *testing.T) {
	fixture := httptest.NewTLSServer(nil)
	defer fixture.Close()
	host, portText, _ := net.SplitHostPort(fixture.Listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	body, err := json.Marshal(map[string]any{
		"mail_backend": "qqimap", "domain": "my-owned-domain.test",
		"qq_imap_host": host, "qq_imap_port": port,
		"qq_imap_user": "test@qq.com", "qq_imap_auth_code": "test-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := newEmptyServer(t)
	rec := do(t, s, http.MethodPost, "/admin/api/register/mailbox-test", "", string(body))
	// A private fixture certificate is intentionally untrusted. Reaching this
	// error proves the endpoint checked the connection, not a dummy email domain.
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "IMAP TLS 连接失败") {
		t.Fatalf("configured domain blocked connection test: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "test-secret") || len(s.Registers.list()) != 0 {
		t.Fatal("connection check leaked credentials or created a registration job")
	}
}

func TestMailboxTestRequiresAdminAndValidConfig(t *testing.T) {
	s := newEmptyServer(t)
	s.Store.UpdateSettings(func(settings *store.Settings) { settings.AdminPassword = "admin" })
	path := "/admin/api/register/mailbox-test"
	if rec := do(t, s, http.MethodPost, path, "", `{}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unprotected mailbox test: %d", rec.Code)
	}
	t.Setenv("QQ_IMAP_USER", "")
	t.Setenv("QQ_IMAP_AUTH_CODE", "")
	for _, path := range []string{path, "/admin/api/register"} {
		rec := do(t, s, http.MethodPost, path, "admin", `{"mail_backend":"qqimap","domain":"example.com"}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "QQ_IMAP_AUTH_CODE") {
			t.Fatalf("missing config %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	if len(s.Registers.list()) != 0 {
		t.Fatal("invalid config started a registration job")
	}
}
