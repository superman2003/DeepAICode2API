package server

import (
	"bytes"
	"encoding/json"
)

func decodeCacheControl(raw json.RawMessage) any {
	var value map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return value
}

func copyCacheControl(destination, source map[string]any) map[string]any {
	if value, ok := source["cache_control"].(map[string]any); ok {
		destination["cache_control"] = value
	}
	if value, ok := source["prompt_cache_breakpoint"]; ok {
		destination["prompt_cache_breakpoint"] = value
	}
	return destination
}

func anthropicSystemContent(raw json.RawMessage) any {
	var blocks []map[string]any
	marked := false
	if json.Unmarshal(raw, &blocks) == nil {
		for _, block := range blocks {
			if block["cache_control"] != nil {
				marked = true
				break
			}
		}
	}
	if !marked {
		if text := anthropicSystemText(raw); text != "" {
			return text
		}
		return nil
	}
	parts := make([]any, 0, len(blocks))
	for _, block := range blocks {
		if block["type"] == "text" {
			if text, ok := block["text"].(string); ok && text != "" {
				parts = append(parts, copyCacheControl(map[string]any{"type": "text", "text": text}, block))
			}
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return parts
}

func anthropicHasCacheControl(req *anthropicRequest) bool {
	if len(req.CacheControl) > 0 {
		return true
	}
	if bytes.Contains(req.System, []byte(`"cache_control"`)) {
		return true
	}
	for _, tool := range req.Tools {
		if len(tool.CacheControl) > 0 {
			return true
		}
	}
	for _, message := range req.Messages {
		if bytes.Contains(message.Content, []byte(`"cache_control"`)) {
			return true
		}
	}
	return false
}
