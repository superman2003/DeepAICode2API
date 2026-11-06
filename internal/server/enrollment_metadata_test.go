package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/register"
	"deepaicode2api/internal/store"
)

// Cover the local console path and the CLI HTTP path, persistence, and the real
// outgoing headers used by both quota queries and Anthropic model requests.
func TestEnrollmentPreservesRegistrationMetadata(t *testing.T) {
	for _, mode := range []string{"console", "http"} {
		t.Run(mode, func(t *testing.T) {
			upstream := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/deepai_code/theos" {
					_, _ = io.WriteString(w, `{"enabled":true,"balance":"1","granted":"1"}`)
					return
				}
				_, _ = io.WriteString(w, `{"id":"test","choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			})
			s := newEmptyServer(t)
			s.quotaReprobeDelays = nil
			record := &register.Record{
				Email: "metadata@example.com", APIKey: "fake-upstream-key", Group: "default",
				MachineID:   "12345678-1234-4234-8234-123456789012",
				Fingerprint: "test-browser", UserAgent: "registered-agent",
				AcceptLanguage: "zh-CN,zh;q=0.9", SecCHUA: `"Example";v="1"`, Platform: `"Windows"`,
				TheosBalance: "1", TheosGranted: "1", EmailVerified: true,
			}
			var id string
			var err error
			if mode == "console" {
				id, err = (localEnroller{s: s}).Enroll(context.Background(), record)
			} else {
				gateway := httptest.NewServer(s.Handler())
				defer gateway.Close()
				id, err = register.NewHTTPEnroller(gateway.URL, "").Enroll(context.Background(), record)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Store.Save(); err != nil {
				t.Fatal(err)
			}
			reopened, err := store.Open(s.Store.Path())
			if err != nil {
				t.Fatal(err)
			}
			account := reopened.AccountCopy(id)
			if account == nil || account.MachineID != record.MachineID || account.UserAgent != record.UserAgent || account.AcceptLanguage != record.AcceptLanguage || account.SecCHUA != record.SecCHUA || account.Platform != record.Platform {
				t.Fatal("registration request metadata was lost on enrollment or restart")
			}
			restarted := New(reopened)
			restarted.Up.Origin = upstream.srv.URL
			if _, err := restarted.clientFor(account).Theos(context.Background(), account); err != nil {
				t.Fatal(err)
			}
			assertHeaders := func() {
				request := upstream.last(t)
				want := map[string]string{deepai.HeaderMachine: deepai.MachineFingerprint(record.MachineID), "User-Agent": record.UserAgent, "Accept-Language": record.AcceptLanguage, "sec-ch-ua": record.SecCHUA, "sec-ch-ua-platform": record.Platform}
				for name, value := range want {
					if request.Header.Get(name) != value {
						t.Errorf("%s: got %q, want preserved registration value", name, request.Header.Get(name))
					}
				}
			}
			assertHeaders()
			key := reopened.AddKey("metadata-test", "default").Key
			response := do(t, restarted, http.MethodPost, "/v1/messages", key, `{"model":"glm-5.3-flash","max_tokens":16,"messages":[{"role":"user","content":"Hi"}]}`)
			if response.Code != http.StatusOK {
				t.Fatalf("messages: %d %s", response.Code, response.Body.String())
			}
			assertHeaders()
		})
	}
}
