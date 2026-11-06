package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"deepaicode2api/internal/deepai"
)

type clientKeyContext struct{}
type sessionAccountBinding struct {
	accountID string
	expires   time.Time
}

func sessionBindingKey(ctx context.Context, group, affinity string) string {
	client, _ := ctx.Value(clientKeyContext{}).(string)
	hash := sha256.Sum256([]byte(client + "\x00" + group + "\x00" + affinity))
	return hex.EncodeToString(hash[:])
}

func (s *Server) cachedSessionAccount(ctx context.Context, group, affinity string) *deepai.Account {
	key := sessionBindingKey(ctx, group, affinity)
	s.sessionMu.Lock()
	binding, ok := s.sessionAccounts[key]
	if ok && time.Now().After(binding.expires) {
		delete(s.sessionAccounts, key)
		ok = false
	}
	s.sessionMu.Unlock()
	if !ok {
		return nil
	}
	account := s.Store.AccountCopy(binding.accountID)
	if account == nil || !account.Enabled || account.Failures > 0 || (group != "" && !strings.EqualFold(account.GroupName(), group)) {
		return nil
	}
	return account
}

func (s *Server) rememberSessionAccount(ctx context.Context, group, affinity, id string) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	if s.sessionAccounts == nil {
		s.sessionAccounts = make(map[string]sessionAccountBinding)
	}
	if len(s.sessionAccounts) >= 4096 {
		oldestKey := ""
		var oldest time.Time
		for key, binding := range s.sessionAccounts {
			if oldestKey == "" || binding.expires.Before(oldest) {
				oldestKey, oldest = key, binding.expires
			}
		}
		delete(s.sessionAccounts, oldestKey)
	}
	s.sessionAccounts[sessionBindingKey(ctx, group, affinity)] = sessionAccountBinding{accountID: id, expires: time.Now().Add(30 * time.Minute)}
}

// Preserve explicit cache/session IDs. Otherwise derive an opaque client-scoped
// key from the Claude metadata or stable prompt prefix, never the changing turn.
func prepareClientCache(r *http.Request, payload map[string]any, kind string, metadata json.RawMessage, cacheRequested bool) string {
	if explicit := affinityFor(r, payload); explicit != "" {
		payload["prompt_cache_key"] = explicit
		return explicit
	}
	seed := ""
	for _, header := range []string{"session_id", "x-codex-session-id", "x-session-id"} {
		if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
			seed = header + ":" + value
			break
		}
	}
	if seed == "" && len(metadata) > 0 {
		var values map[string]any
		if json.Unmarshal(metadata, &values) == nil {
			for _, field := range []string{"session_id", "user_id"} {
				if value, ok := values[field].(string); ok && value != "" {
					seed = field + ":" + value
					break
				}
			}
		}
	}
	if seed == "" && cacheRequested {
		prefix := map[string]any{"model": payload["model"], "tools": payload["tools"]}
		if messages, ok := payload["messages"].([]any); ok && len(messages) > 0 {
			if first, ok := messages[0].(map[string]any); ok && (first["role"] == "system" || first["role"] == "developer") {
				prefix["system"] = first["content"]
			}
		}
		raw, _ := json.Marshal(prefix)
		seed = string(raw)
	}
	if seed == "" {
		return ""
	}
	hash := sha256.Sum256([]byte(kind + "\x00" + deepai.KeyFingerprint(extractKey(r)) + "\x00" + seed))
	key := kind + "-" + hex.EncodeToString(hash[:])
	payload["prompt_cache_key"] = key
	return key
}

func anthropicUsage(usage Usage) map[string]any {
	uncached := usage.PromptTokens - usage.CacheReadTokens - usage.CacheCreationTokens
	if uncached < 0 {
		uncached = 0
	}
	result := map[string]any{"input_tokens": uncached, "output_tokens": usage.CompletionTokens, "cache_read_input_tokens": usage.CacheReadTokens, "cache_creation_input_tokens": usage.CacheCreationTokens}
	if usage.CacheCreation5mTokens > 0 || usage.CacheCreation1hTokens > 0 {
		result["cache_creation"] = map[string]any{"ephemeral_5m_input_tokens": usage.CacheCreation5mTokens, "ephemeral_1h_input_tokens": usage.CacheCreation1hTokens}
	}
	return result
}

func responsesUsage(usage Usage) map[string]any {
	return map[string]any{"input_tokens": usage.PromptTokens, "output_tokens": usage.CompletionTokens, "total_tokens": usage.TotalTokens,
		"input_tokens_details":  map[string]any{"cached_tokens": usage.CacheReadTokens, "cache_write_tokens": usage.CacheCreationTokens},
		"output_tokens_details": map[string]any{"reasoning_tokens": usage.ReasoningTokens}}
}
