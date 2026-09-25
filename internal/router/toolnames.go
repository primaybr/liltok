package router

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

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

// declaredToolParams returns each declared tool's parameter names mapped to their JSON schema type.
func declaredToolParams(tools []interface{}) map[string]map[string]string {
	params := make(map[string]map[string]string, len(tools))
	for _, t := range tools {
		tm, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := tm["name"].(string)
		schema, _ := tm["input_schema"].(map[string]interface{})
		if fn, ok := tm["function"].(map[string]interface{}); ok {
			name, _ = fn["name"].(string)
			schema, _ = fn["parameters"].(map[string]interface{})
		}
		if name == "" {
			continue
		}
		props, _ := schema["properties"].(map[string]interface{})
		types := make(map[string]string, len(props))
		for p, def := range props {
			typ := ""
			if dm, ok := def.(map[string]interface{}); ok {
				typ, _ = dm["type"].(string)
			}
			types[p] = typ
		}
		params[name] = types
	}
	return params
}

// Gemini models sometimes write a function call into their text instead of calling it, as
// call:default_api:Grep{path:internal/proxy/,pattern:Foo} with unquoted keys and values.
var leakedCallRegex = regexp.MustCompile(`call:(?:default_api:)?([A-Za-z_][A-Za-z0-9_]*)\{`)

// extractLeakedCalls converts call:Name{...} text into tool calls for declared tools. Arguments are
// split only at the tool's own parameter names, so commas inside values survive. leaked reports
// call syntax that could not be converted; the caller should fail over rather than return it as text.
func extractLeakedCalls(content string, tools []interface{}) (clean string, calls []provider.UnifiedToolCall, leaked bool) {
	locs := leakedCallRegex.FindAllStringSubmatchIndex(content, -1)
	if len(locs) == 0 {
		return content, nil, false
	}
	params := declaredToolParams(tools)
	folded := make(map[string]string, len(params))
	for n := range params {
		folded[foldToolName(n)] = n
	}

	for i, loc := range locs {
		end := len(content)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		body := strings.TrimSpace(content[loc[3]+1 : end])
		name, ok := folded[foldToolName(content[loc[2]:loc[3]])]
		if !ok || !strings.HasSuffix(body, "}") {
			return content, nil, true
		}
		args, ok := parseLeakedArgs(strings.TrimSuffix(body, "}"), params[name])
		if !ok {
			return content, nil, true
		}
		argBytes, _ := json.Marshal(args)
		call := provider.UnifiedToolCall{ID: fmt.Sprintf("call_leaked_%x_%d", time.Now().UnixNano(), i), Type: "function"}
		call.Function.Name = name
		call.Function.Arguments = string(argBytes)
		calls = append(calls, call)
	}
	return strings.TrimSpace(content[:locs[0][0]]), calls, false
}

func parseLeakedArgs(body string, types map[string]string) (map[string]interface{}, bool) {
	args := map[string]interface{}{}
	if strings.TrimSpace(body) == "" {
		return args, true
	}
	if err := json.Unmarshal([]byte("{"+body+"}"), &args); err == nil {
		return args, true
	}
	if len(types) == 0 {
		return nil, false
	}
	names := make([]string, 0, len(types))
	for p := range types {
		names = append(names, regexp.QuoteMeta(p))
	}
	keyRegex := regexp.MustCompile(`(?:^|,)\s*"?(` + strings.Join(names, "|") + `)"?\s*:`)
	keys := keyRegex.FindAllStringSubmatchIndex(body, -1)
	if len(keys) == 0 || keys[0][0] != 0 {
		return nil, false
	}
	for i, k := range keys {
		end := len(body)
		if i+1 < len(keys) {
			end = keys[i+1][0]
		}
		key := body[k[2]:k[3]]
		raw := strings.TrimSpace(body[k[1]:end])
		if unq, err := strconv.Unquote(raw); err == nil {
			raw = unq
		}
		var val interface{} = raw
		if types[key] != "string" && types[key] != "" {
			var parsed interface{}
			if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
				val = parsed
			}
		}
		args[key] = val
	}
	return args, true
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
