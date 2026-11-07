package deepai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"
)

func TestChatInstructionsCompatibilityPreservesCallerPayload(t *testing.T) {
	for _, role := range []string{"user", "developer", "system"} {
		t.Run(role, func(t *testing.T) {
			input := map[string]any{
				"model": "glm-5.3-flash", "stream": false, "max_tokens": float64(96),
				"messages":         []any{map[string]any{"role": role, "content": []any{map[string]any{"type": "text", "text": "Keep this exact instruction", "cache_control": map[string]any{"type": "ephemeral", "ttl": "1h"}}}}},
				"tools":            []any{map[string]any{"type": "function", "function": map[string]any{"name": "read_file"}}},
				"prompt_cache_key": "session-1",
			}
			raw, _ := json.Marshal(input)
			var got map[string]any
			if err := json.Unmarshal(normalizeChatInstructions(raw), &got); err != nil {
				t.Fatal(err)
			}
			messages := got["messages"].([]any)
			if messages[0].(map[string]any)["role"] != "system" {
				t.Fatal("missing leading system instruction")
			}
			if role == "user" {
				got["messages"] = messages[1:]
			} else if role == "developer" {
				messages[0].(map[string]any)["role"] = "developer"
			}
			if !reflect.DeepEqual(got, input) {
				t.Fatalf("caller content, tools, limits or cache markers changed: %#v", got)
			}
		})
	}
}

func TestOpenModelNormalizesChatOnly(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload map[string]any
		json.Unmarshal(raw, &payload)
		messages := payload["messages"].([]any)
		want := 1
		if r.URL.Path == ModelBasePath+PathChat {
			want = 2
		}
		if len(messages) != want {
			t.Errorf("path %s messages=%d want=%d", r.URL.Path, len(messages), want)
		}
		_, _ = io.WriteString(w, `{}`)
	})
	for _, path := range []string{PathChat, PathResponses} {
		response, err := client.OpenModel(context.Background(), &Account{APIKey: "k"}, path, []byte(`{"messages":[{"role":"user","content":"Hi"}]}`), "")
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
	}
}
