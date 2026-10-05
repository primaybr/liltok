package prune

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCompactToolDefinitions(t *testing.T) {
	toolsJSON := `[
		{
			"type": "function",
			"function": {
				"name": "read_file",
				"description": "Reads the content of a file from disk. This tool supports various encodings, handles binary files, and returns line numbers. Use when needing to inspect source files before making code changes.",
				"parameters": {
					"type": "object",
					"properties": {
						"path": {
							"type": "string",
							"description": "The absolute path or relative path to the file to inspect. Must point to a valid file on the filesystem.",
							"examples": ["/src/main.go", "config.json"]
						}
					},
					"required": ["path"]
				}
			}
		},
		{
			"type": "function",
			"function": {
				"name": "edit_file",
				"description": "Edits a file on disk using string replacement.",
				"parameters": {
					"type": "object",
					"properties": {
						"path": {"type": "string", "description": "Target path."}
					},
					"required": ["path"]
				}
			}
		}
	]`

	var tools []interface{}
	if err := json.Unmarshal([]byte(toolsJSON), &tools); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	// Case 1: edit_file is active, read_file is unused
	activeTools := map[string]bool{"edit_file": true}
	compacted, saved := CompactToolDefinitions(tools, activeTools)
	if saved <= 0 {
		t.Fatalf("expected positive saved bytes, got %d", saved)
	}

	// Verify edit_file was NOT mutated
	editMap := compacted[1].(map[string]interface{})
	editFn := editMap["function"].(map[string]interface{})
	if editFn["description"] != "Edits a file on disk using string replacement." {
		t.Errorf("active tool description was mutated: %v", editFn["description"])
	}

	// Verify read_file was compacted
	readMap := compacted[0].(map[string]interface{})
	readFn := readMap["function"].(map[string]interface{})
	readDesc := readFn["description"].(string)
	if len(readDesc) >= 200 || !strings.HasPrefix(readDesc, "Reads the content of a file") {
		t.Errorf("unused tool description was not compacted properly: %q", readDesc)
	}
	params := readFn["parameters"].(map[string]interface{})
	props := params["properties"].(map[string]interface{})
	pathProp := props["path"].(map[string]interface{})
	if _, hasExamples := pathProp["examples"]; hasExamples {
		t.Errorf("examples array should have been removed from unused tool")
	}
	if req, ok := params["required"].([]interface{}); !ok || len(req) != 1 || req[0] != "path" {
		t.Errorf("required parameters must be preserved, got %v", req)
	}
}

func TestExtractActiveToolNames(t *testing.T) {
	messagesJSON := `[
		{"role": "user", "content": "hello"},
		{
			"role": "assistant",
			"tool_calls": [
				{"function": {"name": "search_code"}}
			]
		},
		{
			"role": "assistant",
			"content": [
				{"type": "tool_use", "name": "run_linter"}
			]
		}
	]`
	var msgs []interface{}
	_ = json.Unmarshal([]byte(messagesJSON), &msgs)

	active := ExtractActiveToolNames(msgs)
	if !active["search_code"] {
		t.Errorf("expected search_code to be active")
	}
	if !active["run_linter"] {
		t.Errorf("expected run_linter to be active")
	}
	if active["other_tool"] {
		t.Errorf("other_tool should not be active")
	}
}
