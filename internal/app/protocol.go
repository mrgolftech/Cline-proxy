package app

import "encoding/json"

func applyAnthropicToolChoice(openAI map[string]any, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var tc map[string]any
	if err := json.Unmarshal(raw, &tc); err != nil {
		return
	}
	kind, _ := tc["type"].(string)
	switch kind {
	case "auto":
		openAI["tool_choice"] = "auto"
	case "any":
		openAI["tool_choice"] = "required"
	case "none":
		openAI["tool_choice"] = "none"
	case "tool":
		if name, _ := tc["name"].(string); name != "" {
			openAI["tool_choice"] = map[string]any{
				"type": "function",
				"function": map[string]any{"name": name},
			}
		}
	}
	if disable, _ := tc["disable_parallel_tool_use"].(bool); disable {
		openAI["parallel_tool_calls"] = false
	}
}
