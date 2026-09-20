package prune

import (
	"encoding/json"
	"fmt"
	"strings"
)

// SessionCompactorOptions specifies the tuning parameters for historical tool result pruning.
type SessionCompactorOptions struct {
	Enabled           bool `json:"enabled"`
	RecentTurnsToKeep int  `json:"recent_turns_to_keep"` // Default: 10
	HeadBytes         int  `json:"head_bytes"`           // Default: 1500
	TailBytes         int  `json:"tail_bytes"`           // Default: 1500
	MinSizeBytes      int  `json:"min_size_bytes"`       // Minimum size before compaction kicks in, default: 4000
	ProtectCodeFiles  bool `json:"protect_code_files"`   // Exempt code and file inspection tools from compaction, default: true
}

// DefaultSessionCompactorOptions returns baseline production configuration.
func DefaultSessionCompactorOptions() SessionCompactorOptions {
	return SessionCompactorOptions{
		Enabled:           true,
		RecentTurnsToKeep: 10,
		HeadBytes:         1500,
		TailBytes:         1500,
		MinSizeBytes:      4000,
		ProtectCodeFiles:  true,
	}
}

// CompactSessionPayload parses JSON messages and compacts historical tool results older than recent turns.
// Returns the pruned JSON bytes, bytes saved, and error if any.
func CompactSessionPayload(raw []byte, opts SessionCompactorOptions) ([]byte, int, error) {
	if !opts.Enabled || len(raw) == 0 {
		return raw, 0, nil
	}

	if opts.RecentTurnsToKeep <= 0 {
		opts.RecentTurnsToKeep = 10
	}
	if opts.HeadBytes <= 0 {
		opts.HeadBytes = 1500
	}
	if opts.TailBytes <= 0 {
		opts.TailBytes = 1500
	}
	if opts.MinSizeBytes <= 0 {
		opts.MinSizeBytes = opts.HeadBytes + opts.TailBytes + 100
	}

	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return raw, 0, err
	}

	rawMsgs, ok := root["messages"].([]interface{})
	if !ok || len(rawMsgs) == 0 {
		return raw, 0, nil
	}

	// 1. Identify turn boundaries.
	// In Anthropic/OpenAI, a "user turn" marks the start of a user interaction or tool feedback loop.
	// We collect indices of all messages with role == "user" or role == "tool".
	userIndices := make([]int, 0)
	for i, m := range rawMsgs {
		msgMap, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msgMap["role"].(string)
		if role == "user" || role == "tool" {
			userIndices = append(userIndices, i)
		}
	}

	// If fewer than or equal to RecentTurnsToKeep user turns, nothing is historical.
	if len(userIndices) <= opts.RecentTurnsToKeep {
		return raw, 0, nil
	}

	// Cutoff is the message index of the (N - RecentTurnsToKeep)-th user turn.
	// Any message before cutoffIndex is eligible for historical compaction.
	cutoffIndex := userIndices[len(userIndices)-opts.RecentTurnsToKeep]

	// Build tool name map from assistant turns: tool_use_id / tool_call_id -> tool_name
	toolNameMap := buildToolNameMap(rawMsgs)

	totalSaved := 0
	modified := false

	for i := 0; i < cutoffIndex; i++ {
		msgMap, ok := rawMsgs[i].(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msgMap["role"].(string)

		// Anthropic style: role == "user", content has blocks with type: "tool_result"
		if role == "user" {
			if contentBlocks, ok := msgMap["content"].([]interface{}); ok {
				for j, block := range contentBlocks {
					blockMap, ok := block.(map[string]interface{})
					if !ok {
						continue
					}
					if bType, _ := blockMap["type"].(string); bType == "tool_result" {
						toolUseID, _ := blockMap["tool_use_id"].(string)
						toolName := toolNameMap[toolUseID]

						// Check if protected
						if opts.ProtectCodeFiles && isFileInspectionTool(toolName) {
							continue
						}

						resContent := blockMap["content"]
						switch c := resContent.(type) {
						case string:
							if opts.ProtectCodeFiles && looksLikeSourceCode(c) {
								continue
							}
							compacted, saved := compactHistoricalString(c, opts.HeadBytes, opts.TailBytes, opts.MinSizeBytes)
							if saved > 0 {
								blockMap["content"] = compacted
								contentBlocks[j] = blockMap
								totalSaved += saved
								modified = true
							}
						case []interface{}:
							for k, part := range c {
								partMap, ok := part.(map[string]interface{})
								if !ok {
									continue
								}
								if pType, _ := partMap["type"].(string); pType == "text" {
									if textStr, ok := partMap["text"].(string); ok {
										if opts.ProtectCodeFiles && looksLikeSourceCode(textStr) {
											continue
										}
										compacted, saved := compactHistoricalString(textStr, opts.HeadBytes, opts.TailBytes, opts.MinSizeBytes)
										if saved > 0 {
											partMap["text"] = compacted
											c[k] = partMap
											totalSaved += saved
											modified = true
										}
									}
								}
							}
						}
					}
				}
			}
		}

		// OpenAI style: role == "tool", content is string or text blocks
		if role == "tool" {
			toolCallID, _ := msgMap["tool_call_id"].(string)
			toolName := toolNameMap[toolCallID]

			// Check if protected
			if opts.ProtectCodeFiles && isFileInspectionTool(toolName) {
				continue
			}

			if contentStr, ok := msgMap["content"].(string); ok {
				if opts.ProtectCodeFiles && looksLikeSourceCode(contentStr) {
					continue
				}
				compacted, saved := compactHistoricalString(contentStr, opts.HeadBytes, opts.TailBytes, opts.MinSizeBytes)
				if saved > 0 {
					msgMap["content"] = compacted
					rawMsgs[i] = msgMap
					totalSaved += saved
					modified = true
				}
			}
		}
	}

	if !modified {
		return raw, 0, nil
	}

	compactedJSON, err := json.Marshal(root)
	if err != nil {
		return raw, 0, err
	}

	return compactedJSON, totalSaved, nil
}

// buildToolNameMap scans assistant turns to map tool call IDs to tool names.
func buildToolNameMap(rawMsgs []interface{}) map[string]string {
	toolMap := make(map[string]string)
	for _, m := range rawMsgs {
		msgMap, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msgMap["role"].(string)
		if role != "assistant" {
			continue
		}

		// Anthropic: content block type: "tool_use"
		if contentBlocks, ok := msgMap["content"].([]interface{}); ok {
			for _, block := range contentBlocks {
				blockMap, ok := block.(map[string]interface{})
				if !ok {
					continue
				}
				if bType, _ := blockMap["type"].(string); bType == "tool_use" {
					id, _ := blockMap["id"].(string)
					name, _ := blockMap["name"].(string)
					if id != "" && name != "" {
						toolMap[id] = name
					}
				}
			}
		}

		// OpenAI: tool_calls array
		if toolCalls, ok := msgMap["tool_calls"].([]interface{}); ok {
			for _, tc := range toolCalls {
				tcMap, ok := tc.(map[string]interface{})
				if !ok {
					continue
				}
				id, _ := tcMap["id"].(string)
				if fnMap, ok := tcMap["function"].(map[string]interface{}); ok {
					name, _ := fnMap["name"].(string)
					if id != "" && name != "" {
						toolMap[id] = name
					}
				}
			}
		}
	}
	return toolMap
}

// isFileInspectionTool determines if a tool is responsible for reading files or directories.
func isFileInspectionTool(toolName string) bool {
	lower := strings.ToLower(toolName)
	if lower == "" {
		return false
	}
	fileTools := []string{
		"view", "read", "cat", "open", "browse", "grep", "glob", "ls",
		"list_dir", "find", "search", "inspect", "diff",
	}
	for _, ft := range fileTools {
		if strings.Contains(lower, ft) {
			return true
		}
	}
	if strings.Contains(lower, "file") {
		return true
	}
	return false
}

// looksLikeSourceCode detects code snippets and diffs to prevent accidental compaction.
func looksLikeSourceCode(content string) bool {
	if strings.Contains(content, "diff --git") || (strings.Contains(content, "--- a/") && strings.Contains(content, "+++ b/")) {
		return true
	}

	if strings.Contains(content, "\n 1 |") || strings.Contains(content, "\n1 |") || strings.Contains(content, "   1: ") || strings.Contains(content, "\n1: ") {
		return true
	}

	codeMarkers := []string{
		"<?php",
		"<!DOCTYPE html>",
		"package main",
		"import (\n",
		"public class ",
		"export default function",
		"export interface ",
		"func (",
		"func main()",
		"def __init__(",
		"pragma solidity",
	}
	for _, marker := range codeMarkers {
		if strings.Contains(content, marker) {
			return true
		}
	}

	return false
}

// compactHistoricalString preserves headBytes from the beginning and tailBytes from the end,
// replacing the redundant middle text with an informative indicator.
func compactHistoricalString(input string, headBytes, tailBytes, minSizeBytes int) (string, int) {
	inLen := len(input)
	if inLen < minSizeBytes || inLen <= headBytes+tailBytes+100 {
		return input, 0
	}

	head := input[:headBytes]
	tail := input[inLen-tailBytes:]
	prunedCount := inLen - headBytes - tailBytes

	indicator := fmt.Sprintf("\n... [output truncated: %d bytes of historical CLI log omitted] ...\n", prunedCount)

	result := head + indicator + tail
	saved := inLen - len(result)
	if saved <= 0 {
		return input, 0
	}
	return result, saved
}
