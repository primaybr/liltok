package prune

import (
	"encoding/json"
	"strings"
)

// ExtractActiveToolNames scans a messages slice for all tools that have been invoked
// or returned in the conversation history (both OpenAI and Anthropic schemas).
func ExtractActiveToolNames(messages []interface{}) map[string]bool {
	active := make(map[string]bool)
	for _, m := range messages {
		msgMap, ok := m.(map[string]interface{})
		if !ok {
			continue
		}

		// 1. OpenAI tool_calls in assistant messages
		if tcList, ok := msgMap["tool_calls"].([]interface{}); ok {
			for _, tc := range tcList {
				if tcMap, ok := tc.(map[string]interface{}); ok {
					if fnMap, ok := tcMap["function"].(map[string]interface{}); ok {
						if name, ok := fnMap["name"].(string); ok && name != "" {
							active[name] = true
						}
					}
				}
			}
		}

		// 2. Anthropic content blocks with type "tool_use"
		if contentList, ok := msgMap["content"].([]interface{}); ok {
			for _, part := range contentList {
				if partMap, ok := part.(map[string]interface{}); ok {
					pType, _ := partMap["type"].(string)
					if pType == "tool_use" {
						if name, ok := partMap["name"].(string); ok && name != "" {
							active[name] = true
						}
					}
				}
			}
		}
	}
	return active
}

// CompactToolDefinitions applies Hermes-style progressive disclosure to tool schemas.
// Tools that have been actively invoked in the session are preserved 100% verbatim.
// Unused tools have their verbose documentation strings trimmed to their first sentence
// and redundant metadata stripped while preserving all argument keys, types, and required lists.
func CompactToolDefinitions(tools []interface{}, activeTools map[string]bool) ([]interface{}, int) {
	if len(tools) == 0 {
		return tools, 0
	}

	origBytes, _ := json.Marshal(tools)
	out := make([]interface{}, 0, len(tools))
	modifiedAny := false

	for _, t := range tools {
		tMap, ok := t.(map[string]interface{})
		if !ok {
			out = append(out, t)
			continue
		}

		// Clone tool map so we do not mutate callers
		cloned := cloneMap(tMap)

		// Identify tool name
		toolName := ""
		var schemaMap map[string]interface{}

		if fnMap, ok := cloned["function"].(map[string]interface{}); ok {
			// OpenAI format
			toolName, _ = fnMap["name"].(string)
			if activeTools[toolName] {
				out = append(out, t)
				continue
			}
			clonedFn := cloneMap(fnMap)
			if desc, ok := clonedFn["description"].(string); ok {
				clonedFn["description"] = compactDescription(desc, 140)
			}
			if params, ok := clonedFn["parameters"].(map[string]interface{}); ok {
				clonedFn["parameters"] = compactSchema(params)
			}
			cloned["function"] = clonedFn
			modifiedAny = true
		} else {
			// Anthropic format
			toolName, _ = cloned["name"].(string)
			if activeTools[toolName] {
				out = append(out, t)
				continue
			}
			if desc, ok := cloned["description"].(string); ok {
				cloned["description"] = compactDescription(desc, 140)
			}
			if schema, ok := cloned["input_schema"].(map[string]interface{}); ok {
				schemaMap = schema
				cloned["input_schema"] = compactSchema(schemaMap)
			}
			modifiedAny = true
		}

		out = append(out, cloned)
	}

	if !modifiedAny {
		return tools, 0
	}

	newBytes, _ := json.Marshal(out)
	saved := len(origBytes) - len(newBytes)
	if saved < 0 {
		return tools, 0
	}
	return out, saved
}

func compactDescription(desc string, maxChars int) string {
	desc = strings.TrimSpace(desc)
	if len(desc) <= maxChars {
		return desc
	}

	// Try to truncate at first sentence
	if idx := strings.Index(desc, "."); idx > 20 && idx <= maxChars {
		return strings.TrimSpace(desc[:idx+1])
	}
	// Truncate at word boundary
	trimmed := desc[:maxChars]
	if lastSpace := strings.LastIndex(trimmed, " "); lastSpace > maxChars/2 {
		trimmed = trimmed[:lastSpace]
	}
	return strings.TrimSpace(trimmed) + "..."
}

func compactSchema(schema map[string]interface{}) map[string]interface{} {
	cloned := cloneMap(schema)

	// Remove redundant optional metadata
	delete(cloned, "examples")
	delete(cloned, "example")
	delete(cloned, "$schema")

	// Compact properties
	if props, ok := cloned["properties"].(map[string]interface{}); ok {
		compactedProps := make(map[string]interface{}, len(props))
		for k, v := range props {
			if propMap, ok := v.(map[string]interface{}); ok {
				pCloned := cloneMap(propMap)
				delete(pCloned, "examples")
				delete(pCloned, "example")
				if d, ok := pCloned["description"].(string); ok {
					pCloned["description"] = compactDescription(d, 80)
				}
				compactedProps[k] = pCloned
			} else {
				compactedProps[k] = v
			}
		}
		cloned["properties"] = compactedProps
	}

	return cloned
}

func cloneMap(m map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
