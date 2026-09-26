package miner

import (
	"encoding/json"
	"strings"

	"github.com/primaybr/liltok/internal/cache"
)

// Reasons StarterFilter.Check gives for leaving an entry out of a starter pack.
const (
	RejectNotCurated  = "not_curated_request"
	RejectPromptEdit  = "prompt_mismatch"
	RejectBadResponse = "bad_response"
	RejectEmptyAnswer = "empty_answer"
	RejectPlaceholder = "placeholder_answer"
)

// placeholderAnswerMarker starts the generic answer older builds of cmd/build_starter_cache wrote
// for prompts without a hand-written answer. It says nothing about the question, so it is never packed.
const placeholderAnswerMarker = "To solve this task efficiently, follow standard industry best practices"

// StarterFilter decides which cache entries may go into a starter pack that ships to other users.
// An entry qualifies only when its request is exactly one the miner builds for a curated corpus
// prompt (the Anthropic and OpenAI shapes from computePromptHashes) and its stored prompt still
// hashes to its key. Entries from user traffic can never match, whatever they contain, so no
// session transcript, system prompt or local path can reach a pack through this filter.
type StarterFilter struct {
	prompts []PromptItem
	allowed map[string]map[string]bool // model -> request hashes the miner builds for that model
}

// NewStarterFilter builds a filter over the given prompts; nil means the embedded curated corpus.
func NewStarterFilter(prompts []PromptItem) *StarterFilter {
	if prompts == nil {
		prompts = GetCuratedPrompts("all")
	}
	return &StarterFilter{prompts: prompts, allowed: make(map[string]map[string]bool)}
}

// Check returns "" when the entry may be packed, or one of the Reject* reasons.
func (f *StarterFilter) Check(it CacheExportItem) string {
	if !f.allowedHashes(it.Model)[it.Hash] {
		return RejectNotCurated
	}
	norm, err := cache.NormalizePayload([]byte(it.NormalizedPrompt), cache.NormalizationOptions{CacheNonzeroTemperature: true})
	if err != nil || norm.Hash != it.Hash {
		return RejectPromptEdit
	}
	text, ok := responseText(it.ResponsePayload)
	switch {
	case !ok:
		return RejectBadResponse
	case strings.TrimSpace(text) == "":
		return RejectEmptyAnswer
	case strings.Contains(text, placeholderAnswerMarker):
		return RejectPlaceholder
	}
	return ""
}

func (f *StarterFilter) allowedHashes(model string) map[string]bool {
	if set, ok := f.allowed[model]; ok {
		return set
	}
	set := make(map[string]bool, 2*len(f.prompts))
	for _, p := range f.prompts {
		for h := range computePromptHashes(p, []string{model}) {
			set[h] = true
		}
	}
	f.allowed[model] = set
	return set
}

// responseText extracts the reply text from a stored Anthropic message or OpenAI chat completion.
func responseText(payload string) (string, bool) {
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(payload), &resp); err != nil {
		return "", false
	}
	var b strings.Builder
	for _, c := range resp.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	for _, c := range resp.Choices {
		b.WriteString(c.Message.Content)
	}
	return b.String(), true
}
