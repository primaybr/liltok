package router

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/primaybr/liltok/internal/provider"
)

func TestTranslator_ToolUsePriorityOverMaxTokens(t *testing.T) {
	translator := NewTranslator()
	resp := &provider.UnifiedChatResponse{
		ID:           "test_len_tc",
		Role:         "assistant",
		Content:      "I will run the command.",
		FinishReason: "length",
		ToolCalls: []provider.UnifiedToolCall{
			{
				ID:   "call_1",
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Name:      "Bash",
					Arguments: `{"command": "npm test"}`,
				},
			},
		},
		Usage: provider.UnifiedUsage{
			PromptTokens:     100,
			CompletionTokens: 50,
		},
	}

	raw, err := translator.ConvertOpenAIToAnthropicResponse(resp, "claude-sonnet-5")
	if err != nil {
		t.Fatalf("ConvertOpenAIToAnthropicResponse failed: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	stopReason, _ := parsed["stop_reason"].(string)
	if stopReason != "tool_use" {
		t.Errorf("expected stop_reason 'tool_use', got %q", stopReason)
	}
}

func TestTranslator_UnclosedToolCallRecovery(t *testing.T) {
	translator := NewTranslator()
	content := "I will create the test script now:\n<tool_call>\n{\"name\": \"Bash\", \"arguments\": {\"command\": \"cat << 'EOF' > test.sh\\n#!/bin/bash\\necho 'hello world'\\n"

	resp := &provider.UnifiedChatResponse{
		ID:           "test_unclosed_tc",
		Role:         "assistant",
		Content:      content,
		FinishReason: "length",
		Usage: provider.UnifiedUsage{
			PromptTokens:     150,
			CompletionTokens: 80,
		},
	}

	raw, err := translator.ConvertOpenAIToAnthropicResponse(resp, "claude-sonnet-5")
	if err != nil {
		t.Fatalf("ConvertOpenAIToAnthropicResponse failed: %v", err)
	}

	var parsed struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string                 `json:"type"`
			Text  string                 `json:"text"`
			Name  string                 `json:"name"`
			Input map[string]interface{} `json:"input"`
		} `json:"content"`
	}

	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if parsed.StopReason != "tool_use" {
		t.Errorf("expected stop_reason 'tool_use', got %q", parsed.StopReason)
	}

	var foundTool bool
	for _, b := range parsed.Content {
		if b.Type == "text" {
			if strings.Contains(b.Text, "<tool_call>") || strings.Contains(b.Text, "cat <<") {
				t.Errorf("raw tool call or script leaked into text block: %q", b.Text)
			}
		} else if b.Type == "tool_use" {
			foundTool = true
			if b.Name != "Bash" {
				t.Errorf("expected tool name 'Bash', got %q", b.Name)
			}
			cmd, _ := b.Input["command"].(string)
			if !strings.Contains(cmd, "test.sh") {
				t.Errorf("expected command to contain 'test.sh', got %q", cmd)
			}
			if !strings.Contains(cmd, "EOF") {
				t.Errorf("expected command to close heredoc EOF, got %q", cmd)
			}
		}
	}

	if !foundTool {
		t.Errorf("expected tool_use block in content, found none")
	}
}

func TestTranslator_UnclosedDSMLRecovery(t *testing.T) {
	translator := NewTranslator()
	content := "Executing setup:\n<|DSML|invoke name=\"Bash\"><|DSML|parameter name=\"command\" string=\"true\">cat << 'EOF' > setup.py\nprint('starting')"

	resp := &provider.UnifiedChatResponse{
		ID:           "test_unclosed_dsml",
		Role:         "assistant",
		Content:      content,
		FinishReason: "length",
		Usage: provider.UnifiedUsage{
			PromptTokens:     120,
			CompletionTokens: 60,
		},
	}

	raw, err := translator.ConvertOpenAIToAnthropicResponse(resp, "claude-sonnet-5")
	if err != nil {
		t.Fatalf("ConvertOpenAIToAnthropicResponse failed: %v", err)
	}

	var parsed struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string                 `json:"type"`
			Text  string                 `json:"text"`
			Name  string                 `json:"name"`
			Input map[string]interface{} `json:"input"`
		} `json:"content"`
	}

	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if parsed.StopReason != "tool_use" {
		t.Errorf("expected stop_reason 'tool_use', got %q", parsed.StopReason)
	}

	var foundTool bool
	for _, b := range parsed.Content {
		if b.Type == "text" {
			if strings.Contains(b.Text, "DSML") || strings.Contains(b.Text, "setup.py") {
				t.Errorf("DSML markup or script leaked into text block: %q", b.Text)
			}
		} else if b.Type == "tool_use" {
			foundTool = true
			if b.Name != "Bash" {
				t.Errorf("expected tool name 'Bash', got %q", b.Name)
			}
			cmd, _ := b.Input["command"].(string)
			if !strings.Contains(cmd, "setup.py") {
				t.Errorf("expected command to contain 'setup.py', got %q", cmd)
			}
		}
	}

	if !foundTool {
		t.Errorf("expected tool_use block in content, found none")
	}
}

func TestTranslator_MarkdownJSONToolCall(t *testing.T) {
	content := "Here is the tool invocation:\n```json\n{\n  \"name\": \"Bash\",\n  \"arguments\": {\"command\": \"git status\"}\n}\n```"
	clean, calls := ExtractTextToolCalls(content)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Function.Name != "Bash" {
		t.Errorf("expected tool name 'Bash', got %q", calls[0].Function.Name)
	}
	if strings.Contains(clean, "git status") {
		t.Errorf("expected tool invocation removed from text, got %q", clean)
	}
}

func TestTranslator_MarkdownBashToolCall(t *testing.T) {
	content := "I will create the deployment script now:\n```bash\ncat << 'EOF' > deploy.sh\n#!/bin/bash\nset -e\n./deploy.sh\nEOF\nchmod +x deploy.sh\n./deploy.sh\n```"
	clean, calls := ExtractTextToolCalls(content)
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Function.Name != "Bash" {
		t.Errorf("expected tool name 'Bash', got %q", calls[0].Function.Name)
	}
	if strings.Contains(clean, "deploy.sh") {
		t.Errorf("expected bash script removed from text, got %q", clean)
	}
}

func TestTranslator_ThinkingBlocks(t *testing.T) {
	translator := NewTranslator()
	content := "<think>\nLet's analyze the request and write the script:\ncat << 'EOF' > draft.sh\n</think>\nI am ready to write the code."

	resp := &provider.UnifiedChatResponse{
		ID:           "test_think",
		Role:         "assistant",
		Content:      content,
		FinishReason: "stop",
		Usage: provider.UnifiedUsage{
			PromptTokens:     200,
			CompletionTokens: 100,
		},
	}

	raw, err := translator.ConvertOpenAIToAnthropicResponse(resp, "claude-sonnet-5")
	if err != nil {
		t.Fatalf("ConvertOpenAIToAnthropicResponse failed: %v", err)
	}

	var parsed struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"content"`
	}

	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	var foundThinking bool
	for _, b := range parsed.Content {
		if b.Type == "thinking" {
			foundThinking = true
			if !strings.Contains(b.Thinking, "draft.sh") {
				t.Errorf("expected thinking to contain 'draft.sh', got %q", b.Thinking)
			}
		} else if b.Type == "text" {
			if strings.Contains(b.Text, "<think>") || strings.Contains(b.Text, "draft.sh") {
				t.Errorf("thinking content or script leaked into text block: %q", b.Text)
			}
		}
	}

	if !foundThinking {
		t.Errorf("expected thinking block in content, found none")
	}
}

func TestRepairJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "unclosed string and brace",
			input:    `{"command": "cat << 'EOF' > file.txt\nhello`,
			expected: `file.txt`,
		},
		{
			name:     "unclosed nested braces",
			input:    `{"name": "Bash", "arguments": {"command": "ls`,
			expected: `Bash`,
		},
		{
			name:     "trailing comma",
			input:    `{"a": 1, "b": 2,`,
			expected: `1`,
		},
		{
			name:     "invalid backslash escape",
			input:    `{"path": "C:\path\to\file"}`,
			expected: `file`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repaired := RepairJSON(tt.input)
			var dummy interface{}
			if err := json.Unmarshal([]byte(repaired), &dummy); err != nil {
				t.Errorf("RepairJSON produced invalid JSON for %q: %v -> %s", tt.input, err, repaired)
			}
			if !strings.Contains(repaired, tt.expected) {
				t.Errorf("expected repaired JSON to contain %q, got %s", tt.expected, repaired)
			}
		})
	}
}

func TestTranslator_ExitPlanMode_ArgumentExtraction(t *testing.T) {
	translator := NewTranslator()
	planText := "# Implementation Plan\n\n1. Analyze requirements\n2. Refactor code\n3. Run tests"
	resp := &provider.UnifiedChatResponse{
		ID:           "test_exit_plan",
		Role:         "assistant",
		Content:      "",
		FinishReason: "tool_calls",
		ToolCalls: []provider.UnifiedToolCall{
			{
				ID:   "call_exit_1",
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Name:      "ExitPlanMode",
					Arguments: fmt.Sprintf(`{"plan": %q, "next_action": "continue_coding"}`, planText),
				},
			},
		},
	}

	raw, err := translator.ConvertOpenAIToAnthropicResponse(resp, "claude-sonnet-5")
	if err != nil {
		t.Fatalf("ConvertOpenAIToAnthropicResponse failed: %v", err)
	}

	var parsed struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string                 `json:"type"`
			Text  string                 `json:"text"`
			Name  string                 `json:"name"`
			Input map[string]interface{} `json:"input"`
		} `json:"content"`
	}

	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if parsed.StopReason != "tool_use" {
		t.Errorf("expected stop_reason 'tool_use', got %q", parsed.StopReason)
	}

	var foundText bool
	var foundTool bool
	for _, b := range parsed.Content {
		if b.Type == "text" {
			foundText = true
			if !strings.Contains(b.Text, "Analyze requirements") {
				t.Errorf("expected plan in text content block, got: %q", b.Text)
			}
		} else if b.Type == "tool_use" {
			foundTool = true
			if b.Name != "ExitPlanMode" {
				t.Errorf("expected tool name 'ExitPlanMode', got %q", b.Name)
			}
			if p, ok := b.Input["plan"].(string); !ok || !strings.Contains(p, "Analyze requirements") {
				t.Errorf("expected plan in tool input, got: %v", b.Input)
			}
		}
	}

	if !foundText {
		t.Errorf("expected text block containing plan, found none")
	}
	if !foundTool {
		t.Errorf("expected ExitPlanMode tool_use block, found none")
	}
}

func TestTranslator_ExitPlanMode_NormalizeNameAndDescription(t *testing.T) {
	translator := NewTranslator()
	resp := &provider.UnifiedChatResponse{
		ID:           "test_exit_plan_norm",
		Role:         "assistant",
		Content:      "I have finalized the plan.",
		FinishReason: "tool_calls",
		ToolCalls: []provider.UnifiedToolCall{
			{
				ID:   "call_exit_norm_1",
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Name:      "exit_plan_mode",
					Arguments: `{"description": "## Steps\n- Step A\n- Step B"}`,
				},
			},
		},
	}

	raw, err := translator.ConvertOpenAIToAnthropicResponse(resp, "claude-sonnet-5")
	if err != nil {
		t.Fatalf("ConvertOpenAIToAnthropicResponse failed: %v", err)
	}

	var parsed struct {
		Content []struct {
			Type  string                 `json:"type"`
			Text  string                 `json:"text"`
			Name  string                 `json:"name"`
			Input map[string]interface{} `json:"input"`
		} `json:"content"`
	}

	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	var foundText bool
	var foundTool bool
	for _, b := range parsed.Content {
		if b.Type == "text" {
			foundText = true
			if !strings.Contains(b.Text, "I have finalized the plan.") || !strings.Contains(b.Text, "Step A") {
				t.Errorf("expected text block to contain preamble and plan, got: %q", b.Text)
			}
		} else if b.Type == "tool_use" {
			foundTool = true
			if b.Name != "ExitPlanMode" {
				t.Errorf("expected normalized tool name 'ExitPlanMode', got %q", b.Name)
			}
			if p, ok := b.Input["plan"].(string); !ok || !strings.Contains(p, "Step A") {
				t.Errorf("expected plan in tool input, got: %v", b.Input)
			}
		}
	}

	if !foundText || !foundTool {
		t.Fatalf("missing text (%v) or tool (%v)", foundText, foundTool)
	}
}

func TestTranslator_ExitPlanMode_ThinkingExtraction(t *testing.T) {
	translator := NewTranslator()
	resp := &provider.UnifiedChatResponse{
		ID:           "test_exit_plan_think",
		Role:         "assistant",
		Content:      "<think>Evaluating code structure...\n# Implementation Plan\n1. Add middleware\n2. Verify health\nNow I will call ExitPlanMode.</think>",
		FinishReason: "tool_calls",
		ToolCalls: []provider.UnifiedToolCall{
			{
				ID:   "call_exit_think_1",
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Name:      "ExitPlanMode",
					Arguments: `{}`,
				},
			},
		},
	}

	raw, err := translator.ConvertOpenAIToAnthropicResponse(resp, "claude-sonnet-5")
	if err != nil {
		t.Fatalf("ConvertOpenAIToAnthropicResponse failed: %v", err)
	}

	var parsed struct {
		Content []struct {
			Type  string                 `json:"type"`
			Text  string                 `json:"text"`
			Name  string                 `json:"name"`
			Input map[string]interface{} `json:"input"`
		} `json:"content"`
	}

	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	var foundText bool
	for _, b := range parsed.Content {
		if b.Type == "text" {
			foundText = true
			if !strings.Contains(b.Text, "Add middleware") {
				t.Errorf("expected plan from thinking in text block, got: %q", b.Text)
			}
		}
	}

	if !foundText {
		t.Errorf("expected text block containing extracted plan, found none")
	}
}

func TestTranslator_ExitPlanMode_StepsArray(t *testing.T) {
	translator := NewTranslator()
	resp := &provider.UnifiedChatResponse{
		ID:           "test_exit_plan_steps",
		Role:         "assistant",
		Content:      "",
		FinishReason: "tool_calls",
		ToolCalls: []provider.UnifiedToolCall{
			{
				ID:   "call_exit_steps_1",
				Type: "function",
				Function: struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{
					Name:      "ExitPlanMode",
					Arguments: `{"steps": [{"title": "Setup", "description": "Configure env"}, {"title": "Build", "description": "Compile code"}]}`,
				},
			},
		},
	}

	raw, err := translator.ConvertOpenAIToAnthropicResponse(resp, "claude-sonnet-5")
	if err != nil {
		t.Fatalf("ConvertOpenAIToAnthropicResponse failed: %v", err)
	}

	var parsed struct {
		Content []struct {
			Type  string                 `json:"type"`
			Text  string                 `json:"text"`
			Name  string                 `json:"name"`
			Input map[string]interface{} `json:"input"`
		} `json:"content"`
	}

	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	var foundText bool
	for _, b := range parsed.Content {
		if b.Type == "text" {
			foundText = true
			if !strings.Contains(b.Text, "Setup") || !strings.Contains(b.Text, "Configure env") {
				t.Errorf("expected formatted steps in text block, got: %q", b.Text)
			}
		}
	}

	if !foundText {
		t.Errorf("expected text block containing formatted steps, found none")
	}
}

func TestTranslator_PlanMode_PreservesMarkdownBash(t *testing.T) {
	translator := NewTranslator()
	content := `# Implementation Plan

1. Run unit tests:
` + "```bash\ngo test ./...\n```\n" + `
2. Review coverage.

I will call ExitPlanMode now to proceed.`

	resp := &provider.UnifiedChatResponse{
		ID:           "test_plan_bash",
		Role:         "assistant",
		Content:      content,
		FinishReason: "stop",
	}

	raw, err := translator.ConvertOpenAIToAnthropicResponse(resp, "claude-sonnet-5")
	if err != nil {
		t.Fatalf("ConvertOpenAIToAnthropicResponse failed: %v", err)
	}

	var parsed struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string                 `json:"type"`
			Text  string                 `json:"text"`
			Name  string                 `json:"name"`
			Input map[string]interface{} `json:"input"`
		} `json:"content"`
	}

	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if parsed.StopReason != "tool_use" {
		t.Errorf("expected stop_reason 'tool_use', got %q", parsed.StopReason)
	}

	var foundBashTool bool
	var foundExitPlanTool bool
	var foundTextWithBashCode bool

	for _, b := range parsed.Content {
		if b.Type == "tool_use" {
			if b.Name == "Bash" {
				foundBashTool = true
			}
			if b.Name == "ExitPlanMode" {
				foundExitPlanTool = true
			}
		} else if b.Type == "text" {
			if strings.Contains(b.Text, "```bash") && strings.Contains(b.Text, "go test ./...") {
				foundTextWithBashCode = true
			}
		}
	}

	if foundBashTool {
		t.Errorf("expected markdown bash block inside plan NOT to be converted to Bash tool call")
	}
	if !foundExitPlanTool {
		t.Errorf("expected synthesized ExitPlanMode tool call, found none")
	}
	if !foundTextWithBashCode {
		t.Errorf("expected text block to preserve markdown bash code block intact")
	}
}
