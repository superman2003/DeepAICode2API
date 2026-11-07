package deepai

import "encoding/json"

// Lane 4 expects a leading instruction message even for a minimal Chat request.
// Apply this at the transport boundary so Chat, Messages, Responses bridges and
// registration probes share the same compatibility behavior. Preserve explicit
// instructions and all content blocks, tool fields and cache markers.
func normalizeChatInstructions(body []byte) []byte {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return body
	}
	var messages []json.RawMessage
	if json.Unmarshal(payload["messages"], &messages) != nil || len(messages) == 0 {
		return body
	}
	var first map[string]json.RawMessage
	if json.Unmarshal(messages[0], &first) != nil || first == nil {
		return body
	}
	var role string
	if json.Unmarshal(first["role"], &role) != nil || role == "system" {
		return body
	}
	if role == "developer" {
		first["role"] = json.RawMessage(`"system"`)
		messages[0], _ = json.Marshal(first)
	} else if role == "user" || role == "assistant" || role == "tool" {
		instruction := json.RawMessage(`{"role":"system","content":"You are a helpful assistant."}`)
		messages = append([]json.RawMessage{instruction}, messages...)
	} else {
		return body
	}
	payload["messages"], _ = json.Marshal(messages)
	normalized, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return normalized
}
