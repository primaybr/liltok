package share

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
)

// ExtractInput is one cache entry: its canonical request and the stored response.
type ExtractInput struct {
	NormalizedPrompt string
	ResponsePayload  string
}

// Candidate is a standalone question found in a cache entry. Only the question text and its ID
// are ever carried forward; the answer is read to check it, never kept.
type Candidate struct {
	ID       string
	Question string
}

// Reasons Extract skips an entry.
const (
	SkipNotRequest      = "skip_not_request"
	SkipNoUserText      = "skip_no_user_text"
	SkipToolTurn        = "skip_tool_turn" // the request does not end with a user turn, or that turn carries tool results
	SkipNonTextQuestion = "skip_non_text_question"
	SkipUnknownTag      = "skip_unknown_tag"
	SkipNonTextAnswer   = "skip_non_text_answer"
	SkipContextRef      = "skip_context_ref"
	SkipFollowUp        = "skip_follow_up"
)

// followUpMinWords is the length below which a later user turn is treated as a follow-up that
// depends on the earlier conversation.
const followUpMinWords = 12

var (
	// Blocks clients inject around the user's own words. They are removed whole; free text is never edited.
	wrapperBlocks = []*regexp.Regexp{
		regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`),
		regexp.MustCompile(`(?s)<ide_selection>.*?</ide_selection>`),
		regexp.MustCompile(`(?s)<ide_opened_file>.*?</ide_opened_file>`),
		regexp.MustCompile(`(?s)<command-(?:name|message|args)>.*?</command-(?:name|message|args)>`),
		regexp.MustCompile(`(?s)<local-command-(?:stdout|stderr)>.*?</local-command-(?:stdout|stderr)>`),
		regexp.MustCompile(`(?s)<user-prompt-submit-hook>.*?</user-prompt-submit-hook>`),
	}
	anyTag     = regexp.MustCompile(`</?[A-Za-z][\w:\-]*(?:\s[^<>]*)?>`)
	contextRef = regexp.MustCompile(`(?i)\b(?:this|that|these|above|attached|my|our)\s+(?:file|files|code|snippet|function|method|class|error|errors|output|log|logs|diff|repo|repository|project|test|tests|script|config)\b|\bfix this\b|\bhere'?s\b|\bsee (?:above|below)\b|\bthe above\b|\bas shown\b`)
	whitespace = regexp.MustCompile(`\s+`)
)

// Extract returns the standalone question in an entry, or a skip reason.
func Extract(in ExtractInput) (Candidate, string) {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(in.NormalizedPrompt), &req); err != nil || len(req.Messages) == 0 {
		return Candidate{}, SkipNotRequest
	}
	last := len(req.Messages) - 1
	if req.Messages[last].Role != "user" {
		return Candidate{}, SkipToolTurn
	}
	userTurns := 0
	for _, m := range req.Messages {
		if m.Role == "user" {
			userTurns++
		}
	}

	text, reason := questionText(req.Messages[last].Content)
	if reason != "" {
		return Candidate{}, reason
	}
	for _, re := range wrapperBlocks {
		text = re.ReplaceAllString(text, " ")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return Candidate{}, SkipNoUserText
	}
	if anyTag.MatchString(text) {
		return Candidate{}, SkipUnknownTag
	}
	if !textAnswer(in.ResponsePayload) {
		return Candidate{}, SkipNonTextAnswer
	}
	if contextRef.MatchString(text) {
		return Candidate{}, SkipContextRef
	}
	if userTurns > 1 && len(strings.Fields(text)) < followUpMinWords {
		return Candidate{}, SkipFollowUp
	}
	return Candidate{ID: QuestionID(text), Question: text}, ""
}

// QuestionID identifies a question independent of case and whitespace.
func QuestionID(q string) string {
	n := strings.ToLower(whitespace.ReplaceAllString(strings.TrimSpace(q), " "))
	sum := sha256.Sum256([]byte(n))
	return hex.EncodeToString(sum[:])
}

func questionText(raw json.RawMessage) (string, string) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, ""
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return "", SkipNotRequest
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "tool_result":
			return "", SkipToolTurn
		default:
			return "", SkipNonTextQuestion
		}
	}
	return strings.Join(parts, "\n"), ""
}

// textAnswer reports whether the stored response is a complete text answer with no tool calls.
func textAnswer(payload string) bool {
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Choices    []struct {
			Message struct {
				Content   string            `json:"content"`
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal([]byte(payload), &resp) != nil {
		return false
	}
	if len(resp.Choices) > 0 {
		c := resp.Choices[0]
		return len(resp.Choices) == 1 && len(c.Message.ToolCalls) == 0 && c.FinishReason == "stop" &&
			strings.TrimSpace(c.Message.Content) != ""
	}
	if resp.StopReason != "end_turn" {
		return false
	}
	var b strings.Builder
	for _, c := range resp.Content {
		switch c.Type {
		case "text":
			b.WriteString(c.Text)
		case "thinking", "redacted_thinking":
		default:
			return false
		}
	}
	return strings.TrimSpace(b.String()) != ""
}
