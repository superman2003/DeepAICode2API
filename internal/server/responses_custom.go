package server

import (
	"encoding/json"
	"net/http"
)

type customToolContext struct{}

func customToolNames(payload map[string]any) map[string]bool {
	result := map[string]bool{}
	if tools, ok := payload["tools"].([]any); ok {
		for _, value := range tools {
			if tool, ok := value.(map[string]any); ok && strOf(tool["type"]) == "custom" {
				result[strOf(tool["name"])] = true
			}
		}
	}
	return result
}

func responseCustomTools(r *http.Request) map[string]bool {
	result, _ := r.Context().Value(customToolContext{}).(map[string]bool)
	return result
}

func customToolInput(arguments string) string {
	var parsed map[string]any
	if json.Unmarshal([]byte(arguments), &parsed) == nil {
		if input, ok := parsed["input"].(string); ok {
			return input
		}
	}
	return arguments
}

func rewriteCustomToolOutput(response map[string]any, custom map[string]bool) {
	items, _ := response["output"].([]any)
	for _, value := range items {
		if item, ok := value.(map[string]any); ok && strOf(item["type"]) == "function_call" && custom[strOf(item["name"])] {
			item["type"] = "custom_tool_call"
			item["input"] = customToolInput(strOf(item["arguments"]))
			delete(item, "arguments")
		}
	}
}
