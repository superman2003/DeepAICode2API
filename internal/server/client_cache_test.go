package server

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"deepaicode2api/internal/deepai"
)

func TestClaudeCacheControlsUsageAndAccounting(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"content":"OK"}}],"usage":{"prompt_tokens":100,"completion_tokens":7,"total_tokens":107,"prompt_tokens_details":{"cached_tokens":60,"cache_write_tokens":20}}}`)
	})
	s, key := newTestServer(t, u)
	body := `{"model":"glm-5.3-flash","max_tokens":100,"metadata":{"user_id":"claude-session-A"},"cache_control":{"type":"ephemeral"},"system":[{"type":"text","text":"stable prefix","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}]}],"tools":[{"name":"lookup","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}]}`
	rec := do(t, s, http.MethodPost, "/v1/messages", key, body)
	if rec.Code != 200 {
		t.Fatalf("request failed: %d %s", rec.Code, rec.Body.String())
	}
	up := u.last(t)
	if up.Payload["cache_control"] == nil || up.Payload["prompt_cache_key"] == nil {
		t.Fatal("root cache controls/session key were dropped")
	}
	if up.Header.Get(deepai.HeaderAffinity) != up.Payload["prompt_cache_key"] {
		t.Fatal("body and session affinity disagree")
	}
	messages := up.Payload["messages"].([]any)
	for _, index := range []int{0, 1} {
		parts, ok := messages[index].(map[string]any)["content"].([]any)
		if !ok || parts[0].(map[string]any)["cache_control"] == nil {
			t.Fatalf("cache breakpoint dropped from message %d", index)
		}
	}
	tools := up.Payload["tools"].([]any)
	if tools[0].(map[string]any)["cache_control"] == nil {
		t.Fatal("tool cache marker dropped")
	}
	out := decodeJSON[map[string]any](t, rec.Body.Bytes())
	usage := out["usage"].(map[string]any)
	if usage["input_tokens"] != float64(20) || usage["cache_read_input_tokens"] != float64(60) || usage["cache_creation_input_tokens"] != float64(20) {
		t.Fatalf("cache counters double-counted or dropped: %#v", usage)
	}
	stats := s.Store.Stats()
	if stats.TotalTokensIn != 100 || stats.TotalCacheReadTokens != 60 || stats.TotalCacheCreationTokens != 20 {
		t.Fatalf("cache accounting lost: %#v", stats)
	}
}

func TestClaudeStreamReportsCacheUsageAtCompletion(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{`{"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}]}`, `{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":80}}}`, `[DONE]`} {
			_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
		}
	})
	s, key := newTestServer(t, u)
	rec := do(t, s, http.MethodPost, "/v1/messages", key, `{"model":"glm-5.3-flash","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	var final map[string]any
	for _, event := range sseEvents(t, rec.Body.String()) {
		if event.Event == "message_delta" {
			final = event.JSON(t)["usage"].(map[string]any)
		}
	}
	if final == nil || final["input_tokens"] != float64(20) || final["cache_read_input_tokens"] != float64(80) {
		t.Fatalf("stream cache usage missing: %#v", final)
	}
	options, _ := u.last(t).Payload["stream_options"].(map[string]any)
	if options["include_usage"] != true {
		t.Fatal("stream did not request upstream usage")
	}
}

func TestUsageUnderstandsAnthropicAndResponsesCacheSemantics(t *testing.T) {
	anthropic := parseUsage(map[string]any{"input_tokens": 10, "output_tokens": 5, "cache_read_input_tokens": 60, "cache_creation_input_tokens": 30})
	if anthropic.PromptTokens != 100 || anthropic.TotalTokens != 105 {
		t.Fatalf("Anthropic input total incorrect: %#v", anthropic)
	}
	responses := parseUsage(map[string]any{"input_tokens": 100, "output_tokens": 5, "input_tokens_details": map[string]any{"cached_tokens": 60, "cache_write_tokens": 30}})
	if responses.PromptTokens != 100 || responses.TotalTokens != 105 || responses.CacheReadTokens != 60 || responses.CacheCreationTokens != 30 {
		t.Fatalf("Responses input double-counted: %#v", responses)
	}
}

func TestStreamUsageDoesNotEraseEarlierCacheCounters(t *testing.T) {
	usage := mergeStreamUsage(Usage{}, map[string]any{"prompt_tokens": 100, "prompt_tokens_details": map[string]any{"cached_tokens": 80}})
	usage = mergeStreamUsage(usage, map[string]any{"completion_tokens": 7})
	if usage.PromptTokens != 100 || usage.CacheReadTokens != 80 || usage.CompletionTokens != 7 || usage.TotalTokens != 107 {
		t.Fatalf("later output-only frame erased cache/input usage: %#v", usage)
	}
	usage = mergeStreamUsage(usage, map[string]any{"prompt_tokens": 100, "prompt_tokens_details": map[string]any{"cached_tokens": 0}})
	if usage.CacheReadTokens != 0 {
		t.Fatal("explicit upstream zero was replaced by stale nonzero cache data")
	}
}

func TestClaudeSessionsStayOnOneAccountAndClientKeysAreIsolated(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}`)
	})
	s, key1 := newTestServer(t, u)
	s.Store.UpsertAccount(&deepai.Account{ID: "acc-2", APIKey: "second-upstream", Enabled: true, Status: "ok"})
	key2 := s.Store.AddKey("other-client", "").Key
	body := `{"model":"glm-5.3-flash","metadata":{"user_id":"same-session"},"max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`
	invoke := func(key string) (string, string) {
		rec := do(t, s, http.MethodPost, "/v1/messages", key, body)
		if rec.Code != 200 {
			t.Fatal(rec.Body.String())
		}
		request := u.last(t)
		return request.Header.Get(deepai.HeaderKeyName), request.Payload["prompt_cache_key"].(string)
	}
	first, cache1 := invoke(key1)
	second, _ := invoke(key1)
	other, cache2 := invoke(key2)
	if first != second {
		t.Fatal("same session was round-robined across accounts")
	}
	if first == other || cache1 == cache2 {
		t.Fatal("different gateway clients shared a session binding")
	}
	account := s.Store.Account("acc-1")
	account.Enabled = false
	fallback, _ := invoke(key1)
	if fallback == first {
		t.Fatal("disabled preferred account prevented failover")
	}
}

func TestRegistrationCountLimitIsConfigurable(t *testing.T) {
	t.Setenv("DEEPAI2API_REGISTER_MAX_COUNT", "")
	if registrationMaxCount() != 1000 {
		t.Fatal("default max must be 1000")
	}
	s := newEmptyServer(t)
	rec := do(t, s, http.MethodPost, "/admin/api/register", "", `{"count":51,"mail_backend":"invalid-test-backend"}`)
	if strings.Contains(rec.Body.String(), "单次最多") {
		t.Fatal("old 50-account cap is still active")
	}
	rec = do(t, s, http.MethodPost, "/admin/api/register", "", `{"count":1001}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "1000") {
		t.Fatal("configured cap was not enforced")
	}
	t.Setenv("DEEPAI2API_REGISTER_MAX_COUNT", "5000")
	if registrationMaxCount() != 5000 {
		t.Fatal("max setting was ignored")
	}
	t.Setenv("DEEPAI2API_REGISTER_MAX_COUNT", "999999")
	if registrationMaxCount() != 10000 {
		t.Fatal("absolute memory-safety cap was not enforced")
	}
	if len(s.Registers.list()) != 0 {
		t.Fatal("validation tests launched registration jobs")
	}
}
