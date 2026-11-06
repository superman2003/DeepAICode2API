package register

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTempMailLOLCreateReceiveAndRestore(t *testing.T) {
	now := time.Now()
	const link = "https://api.deepai.org/verify_email?key=abcd-1234"
	const token = "inbox-secret+with/special?characters"
	var creates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-api-key" {
			t.Error("missing API authentication")
		}
		switch r.URL.Path {
		case "/v2/inbox/create":
			creates.Add(1)
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
				t.Error("wrong create request")
			}
			var body map[string]string
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["prefix"] == "" {
				t.Error("missing generated prefix")
			}
			if body["domain"] != "" {
				t.Error("random-domain request unexpectedly used a domain")
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"address": "fresh@temp.example", "token": token})
		case "/v2/inbox":
			if r.URL.Query().Get("token") != token {
				t.Error("wrong or incorrectly escaped inbox token")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"expired": false, "emails": []map[string]any{
				{"to": "fresh@temp.example", "body": "https://api.deepai.org/verify_email?key=dead-1111", "date": now.Add(-time.Hour).UnixMilli()},
				{"to": "other@temp.example", "body": "https://api.deepai.org/verify_email?key=dead-2222", "date": now.UnixMilli()},
				{"from": "noreply@deepai.org", "to": "fresh@temp.example", "subject": "Verify email", "body": "", "html": "<a href=\"https://api.deepai.org/verify_email&#63;key=abcd-1234\">verify</a>", "date": fmt.Sprint(now.UnixMilli())},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cfg := &Config{TempMailLOLBase: srv.URL + "/v2", TempMailLOLKey: "fake-api-key", TempMailLOLTokenDir: t.TempDir()}
	provider, err := NewProvider(cfg, "tempmail_lol", "", "")
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := provider.Create(context.Background())
	if err != nil || !inbox.Fresh() || inbox.Provider() != "tempmail_lol" {
		t.Fatalf("create: %v %v", inbox, err)
	}
	provider.(*tempMailLOLProvider).inboxGate(token).interval = time.Millisecond
	got, err := inbox.WaitForVerification(context.Background(), now.Add(-time.Minute), time.Second, time.Millisecond)
	if err != nil || got != link {
		t.Fatalf("verification: %q %v", got, err)
	}
	// A new provider instance must reopen the saved inbox after a restart.
	restoredProvider, err := NewProvider(cfg, "tempmail_lol", "", inbox.Email())
	if err != nil {
		t.Fatal(err)
	}
	restored, ok := restoredProvider.(InboxProvider).Inbox(inbox.Email())
	if !ok || restored.Fresh() {
		t.Fatal("did not restore persisted inbox")
	}
	if got, err := restored.WaitForVerification(context.Background(), now.Add(-time.Minute), time.Second, time.Millisecond); err != nil || got != link {
		t.Fatalf("restored verification: %q %v", got, err)
	}
	if creates.Load() != 1 {
		t.Error("restoring an inbox created another address")
	}
	if _, err := NewProvider(cfg, "tempmail_lol", "", "unknown@temp.example"); err == nil {
		t.Error("accepted an address without a saved token")
	}
}

func TestTempMailLOLExpiredAndRateLimited(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusGone, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(status)
				if status == http.StatusOK {
					_, _ = w.Write([]byte(`{"expired":true,"emails":[]}`))
				} else {
					_, _ = w.Write([]byte(`{"error":"limited or expired"}`))
				}
			}))
			defer srv.Close()
			p, _ := newTempMailLOLProvider(&Config{TempMailLOLBase: srv.URL, TempMailLOLTokenDir: t.TempDir()}, "")
			m := &tempMailLOLInbox{provider: p, email: "test@example.com", token: "private-token"}
			_, err := m.WaitForVerification(context.Background(), time.Time{}, 30*time.Millisecond, time.Millisecond)
			if status == http.StatusTooManyRequests {
				if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
					t.Fatalf("rate limit ignored: calls=%d err=%v", calls.Load(), err)
				}
				_, err = p.request(context.Background(), http.MethodGet, "/inbox?token=private-token", nil, "private-token")
				var apiErr *tempMailLOLAPIError
				if !errors.As(err, &apiErr) || apiErr.retryAfter != 7*time.Second {
					t.Fatalf("missing Retry-After: %v", err)
				}
			} else if !errors.Is(err, errTempMailLOLExpired) {
				t.Fatalf("expiry not recognized: %v", err)
			}
		})
	}
}

func TestTempMailLOLSharedInboxPacingAndKeyCooldown(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("token") == "limited" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"Rate Limited"}`))
			return
		}
		_, _ = w.Write([]byte(`{"expired":false,"emails":[]}`))
	}))
	defer srv.Close()
	cfg := &Config{TempMailLOLBase: srv.URL, TempMailLOLKey: "shared-key", TempMailLOLTokenDir: t.TempDir()}
	p1, _ := newTempMailLOLProvider(cfg, "")
	p2, _ := newTempMailLOLProvider(cfg, "")
	p1.inboxGate("shared").interval = 80 * time.Millisecond
	m1 := &tempMailLOLInbox{provider: p1, email: "a@example.com", token: "shared"}
	m2 := &tempMailLOLInbox{provider: p2, email: "a@example.com", token: "shared"}
	if _, err := m1.list(context.Background()); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := m2.list(context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) < 60*time.Millisecond {
		t.Fatal("separate jobs did not share inbox pacing")
	}
	limited := &tempMailLOLInbox{provider: p1, email: "limited@example.com", token: "limited"}
	_, err := limited.list(context.Background())
	var apiErr *tempMailLOLAPIError
	if !errors.As(err, &apiErr) || apiErr.retryAfter < 5*time.Second {
		t.Fatalf("429 without Retry-After did not back off: %v", err)
	}
	before := calls.Load()
	other := &tempMailLOLInbox{provider: p2, email: "b@example.com", token: "other"}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := other.list(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("key cooldown ignored: %v", err)
	}
	if calls.Load() != before {
		t.Fatal("another inbox bypassed the API-key cooldown")
	}
}

func TestTempMailLOLSavedInboxAcceptsOriginalVerification(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"expired": false, "emails": []map[string]any{{"to": "saved@example.com", "body": "https://api.deepai.org/verify_email?key=aaaa-1111", "date": time.Now().Add(-time.Hour).UnixMilli()}}})
	}))
	defer srv.Close()
	p, _ := newTempMailLOLProvider(&Config{TempMailLOLBase: srv.URL, TempMailLOLTokenDir: t.TempDir()}, "")
	m := &tempMailLOLInbox{provider: p, email: "saved@example.com", token: "saved", fresh: false}
	if link, err := m.WaitForVerification(context.Background(), time.Now(), time.Second, time.Second); err != nil || link == "" {
		t.Fatalf("saved verification rejected by retry timestamp: %q %v", link, err)
	}
}

type lolRoundTripFunc func(*http.Request) (*http.Response, error)

func (f lolRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTempMailLOLRedactsSecretsAndRejectsBadResponses(t *testing.T) {
	p, _ := newTempMailLOLProvider(&Config{TempMailLOLKey: "private-api-key", TempMailLOLTokenDir: t.TempDir()}, "")
	const token = "private+inbox/token"
	p.http.Transport = lolRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("failed %s private-api-key", r.URL.String())
	})
	m := &tempMailLOLInbox{provider: p, email: "test@example.com", token: token}
	_, err := m.list(context.Background())
	if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), url.QueryEscape(token)) || strings.Contains(err.Error(), p.key) {
		t.Fatalf("secret leaked in error: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"address":"invalid address","token":"secret"}`))
	}))
	defer srv.Close()
	p, _ = newTempMailLOLProvider(&Config{TempMailLOLBase: srv.URL, TempMailLOLTokenDir: t.TempDir()}, "")
	if _, err := p.Create(context.Background()); err == nil {
		t.Error("accepted invalid created address")
	}
	cfg := &Config{TempMailLOLKey: "configured"}
	if cfg.AutoBackend() != "tempmail_lol" {
		t.Error("configured tempmail.lol was not auto-selected")
	}
}
