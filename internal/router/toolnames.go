package router

import (
	"strings"

	"github.com/primaybr/liltok/internal/provider"
)

// declaredToolNames returns the tool names a request declares, from Anthropic ({"name": ...})
// or OpenAI ({"type": "function", "function": {"name": ...}}) tool schemas.
func declaredToolNames(tools []interface{}) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		tm, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		if n, ok := tm["name"].(string); ok && n != "" {
			names = append(names, n)
			continue
		}
		if fn, ok := tm["function"].(map[string]interface{}); ok {
			if n, ok := fn["name"].(string); ok && n != "" {
				names = append(names, n)
			}
		}
	}
	return names
}

func foldToolName(name string) string {
	return strings.ToLower(strings.ReplaceAll(name, "_", ""))
}

// reconcileToolNames checks each tool call against the request's declared tools. A name that
// differs from exactly one declared tool only by case or underscores is rewritten to that tool's
// name. It returns the first name that matches no declared tool, or "" when all calls resolve.
// Requests that declare no tools are not checked.
func reconcileToolNames(tools []interface{}, calls []provider.UnifiedToolCall) string {
	declared := declaredToolNames(tools)
	if len(declared) == 0 {
		return ""
	}

	exact := make(map[string]bool, len(declared))
	folded := make(map[string][]string, len(declared))
	for _, n := range declared {
		exact[n] = true
		folded[foldToolName(n)] = append(folded[foldToolName(n)], n)
	}

	for i, tc := range calls {
		if exact[tc.Function.Name] {
			continue
		}
		if matches := folded[foldToolName(tc.Function.Name)]; len(matches) == 1 {
			calls[i].Function.Name = matches[0]
			continue
		}
		return tc.Function.Name
	}
	return ""
}
