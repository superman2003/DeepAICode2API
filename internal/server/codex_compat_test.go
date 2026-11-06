package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCodexStreamSequenceCacheAndOutputOrdering(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{
			`{"choices":[{"delta":{"reasoning":"plan"}}]}`,
			`{"choices":[{"delta":{"content":"before"}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-a","function":{"name":"a","arguments":"{}"}},{"index":1,"id":"call-b","function":{"name":"b","arguments":"{}"}}]}}]}`,
			`{"choices":[{"delta":{"content":"after"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":80}}}`,
			`[DONE]`,
		} {
			_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
		}
	})
	s, key := newTestServer(t, u)
	rec := do(t, s, http.MethodPost, "/v1/responses", key, `{"model":"glm-5.3-flash","input":"hi","stream":true,"store":false,"prompt_cache_key":"codex-session","parallel_tool_calls":false,"prompt_cache_options":{"ttl":"30m"},"tools":[{"type":"function","name":"a","strict":true,"parameters":{"type":"object"}},{"type":"function","name":"b","parameters":{"type":"object"}}]}`)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	events := sseEvents(t, rec.Body.String())
	var completed map[string]any
	added := map[int]string{}
	for index, event := range events {
		value := event.JSON(t)
		if value["sequence_number"] != float64(index) {
			t.Fatalf("non-monotonic sequence at %s", event.Event)
		}
		if event.Event == "response.output_item.added" {
			added[int(value["output_index"].(float64))] = value["item"].(map[string]any)["id"].(string)
		}
		if event.Event == "response.completed" {
			completed = value["response"].(map[string]any)
		}
	}
	if len(events) < 2 || events[1].Event != "response.in_progress" || completed == nil {
		t.Fatal("required lifecycle events missing")
	}
	output := completed["output"].([]any)
	if len(output) != 5 {
		t.Fatalf("wrong output count: %#v", output)
	}
	for index, item := range output {
		if item.(map[string]any)["id"] != added[index] {
			t.Fatalf("output item %d moved to another index", index)
		}
	}
	first := output[1].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	last := output[4].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	if first != "before" || last != "after" {
		t.Fatalf("text items duplicated: %v %v", first, last)
	}
	if completed["usage"].(map[string]any)["input_tokens_details"].(map[string]any)["cached_tokens"] != float64(80) {
		t.Fatal("cached tokens lost")
	}
	up := u.last(t).Payload
	if up["parallel_tool_calls"] != false || up["prompt_cache_options"] == nil || up["stream_options"] == nil {
		t.Fatal("Codex request options were dropped")
	}
	if up["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["strict"] != true {
		t.Fatal("strict function schema was dropped")
	}
}

func TestCodexIncompleteAndFailedStreamsAreNotSuccessful(t *testing.T) {
	for _, test := range []struct{ name, chunk, terminal, status string }{
		{"length", `{"choices":[{"delta":{"content":"partial"},"finish_reason":"length"}]}`, "response.incomplete", "incomplete"},
		{"error", `{"error":{"message":"capacity failed"}}`, "response.failed", "failed"},
		{"truncated", `{"choices":[{"delta":{"content":"partial"}}]}`, "response.failed", "failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+test.chunk+"\n\n")
			})
			s, key := newTestServer(t, u)
			rec := do(t, s, http.MethodPost, "/v1/responses", key, `{"model":"glm-5.3-flash","input":"hi","stream":true}`)
			events := sseEvents(t, rec.Body.String())
			last := events[len(events)-1]
			if last.Event != test.terminal || last.JSON(t)["response"].(map[string]any)["status"] != test.status {
				t.Fatalf("wrong terminal event: %s", last.Event)
			}
		})
	}
}

func TestCodexCustomToolRoundTrip(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "blocking", true: "streaming"}[stream], func(t *testing.T) {
			u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: "+`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"patch-call","function":{"name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\\n*** End Patch\"}"}}]},"finish_reason":"tool_calls"}]}`+"\n\ndata: [DONE]\n\n")
					return
				}
				_, _ = io.WriteString(w, `{"choices":[{"message":{"tool_calls":[{"id":"patch-call","function":{"name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\\n*** End Patch\"}"}}]},"finish_reason":"tool_calls"}]}`)
			})
			s, key := newTestServer(t, u)
			body := `{"model":"glm-5.3-flash","input":[{"type":"custom_tool_call","call_id":"previous","name":"apply_patch","input":"old patch"},{"type":"custom_tool_call_output","call_id":"previous","output":"done"},{"role":"user","content":"next"}],"tools":[{"type":"custom","name":"apply_patch"}],"stream":` + map[bool]string{true: "true", false: "false"}[stream] + `}`
			rec := do(t, s, http.MethodPost, "/v1/responses", key, body)
			if rec.Code != 200 {
				t.Fatal(rec.Body.String())
			}
			var response map[string]any
			if stream {
				for _, event := range sseEvents(t, rec.Body.String()) {
					if event.Event == "response.completed" {
						response = event.JSON(t)["response"].(map[string]any)
					}
				}
			} else {
				response = decodeJSON[map[string]any](t, rec.Body.Bytes())
			}
			item := response["output"].([]any)[0].(map[string]any)
			if item["type"] != "custom_tool_call" || item["input"] != "*** Begin Patch\n*** End Patch" || item["call_id"] != "patch-call" {
				t.Fatalf("custom tool input/type lost: %#v", item)
			}
			up := u.last(t).Payload
			messages := up["messages"].([]any)
			if len(messages) != 3 || messages[1].(map[string]any)["role"] != "tool" {
				t.Fatal("custom tool history was dropped")
			}
			parameters := up["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
			if parameters["properties"].(map[string]any)["input"] == nil {
				t.Fatal("custom tool lacks input wrapper")
			}
		})
	}
}

func TestCodexParallelHistoryAndUnsupportedContinuation(t *testing.T) {
	payload := map[string]any{"input": []any{
		map[string]any{"type": "function_call", "call_id": "a", "name": "a", "arguments": "{}"},
		map[string]any{"type": "function_call", "call_id": "b", "name": "b", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "a", "output": "A"},
		map[string]any{"type": "function_call_output", "call_id": "b", "output": "B"},
	}}
	chat, err := responsesToChatPayload(payload, "glm-5.3-flash")
	if err != nil {
		t.Fatal(err)
	}
	messages := chat["messages"].([]any)
	if len(messages) != 3 || len(messages[0].(map[string]any)["tool_calls"].([]any)) != 2 {
		t.Fatal("parallel calls split into invalid assistant messages")
	}
	s, key := newTestServer(t, nil)
	rec := do(t, s, http.MethodPost, "/v1/responses", key, `{"model":"glm-5.3-flash","previous_response_id":"resp-old","input":"continue"}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "previous_response_id") {
		t.Fatal("unsupported continuation silently lost history")
	}
}
