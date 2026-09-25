package prune

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSessionCompactor_AnthropicPayload(t *testing.T) {
	// Create an Anthropic payload with user turns containing tool results.
	// We configure RecentTurnsToKeep: 3 to test clear cutoff.
	largeOutput1 := strings.Repeat("A_START_LINE\nMiddle padding data that is repeated many times to simulate large output.\n", 30) + "FINAL_EXIT_CODE_0"
	largeOutput2 := strings.Repeat("B_START_LINE\nAnother large bash command output that is very long.\n", 30) + "FINAL_STATUS_OK"
	recentOutput := "Recent output from a bash command that must stay untouched."

	messages := []map[string]interface{}{
		// Turn 1 (Historical)
		{
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{
					"type":        "tool_result",
					"tool_use_id": "call_1",
					"content":     largeOutput1,
				},
			},
		},
		{
			"role": "assistant",
			"content": []interface{}{
				map[string]interface{}{
					"type": "tool_use",
					"id":   "call_2",
					"name": "Bash",
				},
			},
		},
		// Turn 2 (Historical)
		{
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{
					"type":        "tool_result",
					"tool_use_id": "call_2",
					"content":     largeOutput2,
				},
			},
		},
		{"role": "assistant", "content": "Acknowledged step 2."},
		// Turn 3 (Recent - untouched)
		{
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{
					"type":        "tool_result",
					"tool_use_id": "call_3",
					"content":     recentOutput,
				},
			},
		},
		{"role": "assistant", "content": "Acknowledged step 3."},
		// Turn 4 (Recent - untouched)
		{
			"role":    "user",
			"content": "Please proceed to step 4.",
		},
		{"role": "assistant", "content": "Acknowledged step 4."},
		// Turn 5 (Recent - untouched)
		{
			"role":    "user",
			"content": "Final instruction.",
		},
	}

	payloadMap := map[string]interface{}{
		"model":    "claude-sonnet-5",
		"messages": messages,
	}
	rawBytes, _ := json.Marshal(payloadMap)

	opts := SessionCompactorOptions{
		Enabled:           true,
		RecentTurnsToKeep: 3,
		HeadBytes:         50,
		TailBytes:         50,
		MinSizeBytes:      200,
		ProtectCodeFiles:  true,
	}

	compacted, saved, err := CompactSessionPayload(rawBytes, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if saved <= 0 {
		t.Fatalf("expected saved bytes > 0, got %d", saved)
	}

	compactedStr := string(compacted)

	// Verify that historical tool results were compacted with clean indicator
	if !strings.Contains(compactedStr, "output truncated:") || !strings.Contains(compactedStr, "historical CLI log omitted") {
		t.Errorf("expected pruned indicator in historical output")
	}
	// Verify that head and tail were preserved
	if !strings.Contains(compactedStr, "A_START_LINE") || !strings.Contains(compactedStr, "FINAL_EXIT_CODE_0") {
		t.Errorf("expected head and tail to be preserved in Turn 1")
	}
	if !strings.Contains(compactedStr, "B_START_LINE") || !strings.Contains(compactedStr, "FINAL_STATUS_OK") {
		t.Errorf("expected head and tail to be preserved in Turn 2")
	}

	// Verify that recent output was NOT pruned
	if !strings.Contains(compactedStr, recentOutput) {
		t.Errorf("expected recent tool output to remain completely untouched")
	}
}

func TestSessionCompactor_ProtectCodeFiles_ViewTool(t *testing.T) {
	// When tool is "View" or "read_file", output must remain 100% untouched even if historical and huge.
	hugeCodeOutput := strings.Repeat("func ProcessItem(i int) {\n\t// detailed code line\n}\n", 50)

	messages := []map[string]interface{}{
		{
			"role": "assistant",
			"content": []interface{}{
				map[string]interface{}{
					"type": "tool_use",
					"id":   "tool_view_1",
					"name": "View",
				},
			},
		},
		{
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{
					"type":        "tool_result",
					"tool_use_id": "tool_view_1",
					"content":     hugeCodeOutput,
				},
			},
		},
		{"role": "assistant", "content": "Seen code."},
		{"role": "user", "content": "turn 2"},
		{"role": "assistant", "content": "turn 2 ack"},
		{"role": "user", "content": "turn 3"},
		{"role": "assistant", "content": "turn 3 ack"},
		{"role": "user", "content": "turn 4"},
	}

	payloadMap := map[string]interface{}{
		"model":    "claude-sonnet-5",
		"messages": messages,
	}
	rawBytes, _ := json.Marshal(payloadMap)

	opts := SessionCompactorOptions{
		Enabled:           true,
		RecentTurnsToKeep: 2,
		HeadBytes:         50,
		TailBytes:         50,
		MinSizeBytes:      200,
		ProtectCodeFiles:  true,
	}

	compacted, saved, err := CompactSessionPayload(rawBytes, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if saved != 0 {
		t.Errorf("expected 0 bytes saved because View tool is protected, got %d", saved)
	}

	if string(compacted) != string(rawBytes) {
		t.Errorf("expected payload to remain byte-for-byte identical when protected")
	}
}

func TestSessionCompactor_ProtectCodeFiles_Heuristic(t *testing.T) {
	// When content looks like source code (e.g. package main, diff, or line numbers), protect it even if tool name unknown.
	diffOutput := "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1,5 +1,5 @@\n" + strings.Repeat("+ newLine\n", 40)

	messages := []map[string]interface{}{
		{
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{
					"type":        "tool_result",
					"tool_use_id": "unknown_tool",
					"content":     diffOutput,
				},
			},
		},
		{"role": "assistant", "content": "diff ack"},
		{"role": "user", "content": "turn 2"},
		{"role": "assistant", "content": "turn 2 ack"},
		{"role": "user", "content": "turn 3"},
	}

	payloadMap := map[string]interface{}{
		"model":    "claude-sonnet-5",
		"messages": messages,
	}
	rawBytes, _ := json.Marshal(payloadMap)

	opts := SessionCompactorOptions{
		Enabled:           true,
		RecentTurnsToKeep: 1,
		HeadBytes:         50,
		TailBytes:         50,
		MinSizeBytes:      100,
		ProtectCodeFiles:  true,
	}

	_, saved, err := CompactSessionPayload(rawBytes, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if saved != 0 {
		t.Errorf("expected 0 bytes saved because diff heuristic protected the output, got %d", saved)
	}
}

func TestSessionCompactor_OpenAIPayload(t *testing.T) {
	largeToolOutput := strings.Repeat("OPENAI_TOOL_LOG_LINE\nSome detailed verbose stack trace or directory tree.\n", 25) + "PROCESS_EXIT_0"

	messages := []map[string]interface{}{
		{"role": "user", "content": "run command"},
		{
			"role": "assistant",
			"tool_calls": []interface{}{
				map[string]interface{}{
					"id":   "call_bash_1",
					"type": "function",
					"function": map[string]interface{}{
						"name": "run_command",
					},
				},
			},
		},
		// Historical tool turn
		{"role": "tool", "tool_call_id": "call_bash_1", "content": largeToolOutput},
		{"role": "assistant", "content": "step 1 done"},
		// Recent turns
		{"role": "user", "content": "run next"},
		{"role": "assistant", "content": "doing next"},
		{"role": "user", "content": "final check"},
	}

	payloadMap := map[string]interface{}{
		"model":    "gpt-4o",
		"messages": messages,
	}
	rawBytes, _ := json.Marshal(payloadMap)

	opts := SessionCompactorOptions{
		Enabled:           true,
		RecentTurnsToKeep: 2,
		HeadBytes:         40,
		TailBytes:         40,
		MinSizeBytes:      200,
		ProtectCodeFiles:  true,
	}

	compacted, saved, err := CompactSessionPayload(rawBytes, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if saved <= 0 {
		t.Fatalf("expected saved bytes > 0, got %d", saved)
	}

	compactedStr := string(compacted)
	if !strings.Contains(compactedStr, "output truncated:") {
		t.Errorf("expected compactor indicator in OpenAI tool message")
	}
	if !strings.Contains(compactedStr, "OPENAI_TOOL_LOG_LINE") || !strings.Contains(compactedStr, "PROCESS_EXIT_0") {
		t.Errorf("expected head and tail to be preserved")
	}
}

func TestSessionCompactor_BelowTurnThreshold(t *testing.T) {
	messages := []map[string]interface{}{
		{"role": "user", "content": "short conversation"},
		{"role": "assistant", "content": "hello"},
	}
	payloadMap := map[string]interface{}{
		"model":    "claude-sonnet-5",
		"messages": messages,
	}
	rawBytes, _ := json.Marshal(payloadMap)

	opts := DefaultSessionCompactorOptions()
	compacted, saved, err := CompactSessionPayload(rawBytes, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if saved != 0 {
		t.Errorf("expected 0 saved bytes for short conversation, got %d", saved)
	}
	if string(compacted) != string(rawBytes) {
		t.Errorf("expected payload to remain identical when below threshold")
	}
}
