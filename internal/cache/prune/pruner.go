package prune

import (
	"encoding/json"
	"math"
)

// PruneOptions configures which compaction strategies are applied.
type PruneOptions struct {
	EnableDiff       bool
	EnableTree       bool
	EnableWhitespace bool
}

// DefaultOptions returns the recommended default pruning configuration.
func DefaultOptions() PruneOptions {
	return PruneOptions{
		EnableDiff:       true,
		EnableTree:       true,
		EnableWhitespace: true,
	}
}

// PruneStats reports the byte savings from pruning.
type PruneStats struct {
	OriginalBytes  int     `json:"original_bytes"`
	PrunedBytes    int     `json:"pruned_bytes"`
	SavedBytes     int     `json:"saved_bytes"`
	ReductionRatio float64 `json:"reduction_ratio"`
}

// Pruner executes token pruning transformations on text and JSON payloads.
type Pruner struct {
	opts PruneOptions
}

// NewPruner creates a new Pruner with given options.
func NewPruner(opts PruneOptions) *Pruner {
	return &Pruner{opts: opts}
}

// PruneText transforms a text string applying enabled compaction filters.
func (p *Pruner) PruneText(text string) (string, int) {
	origLen := len(text)
	if origLen == 0 {
		return text, 0
	}

	curr := text
	if p.opts.EnableDiff {
		curr = CompactDiff(curr)
	}
	if p.opts.EnableTree {
		curr = CompactTree(curr)
	}
	if p.opts.EnableWhitespace {
		curr = StripWhitespaceAndDividers(curr)
	}

	saved := origLen - len(curr)
	if saved < 0 {
		saved = 0
	}
	return curr, saved
}

// PruneJSONPayload intercepts chat completion or messages JSON payloads,
// pruning prompt contents in-place.
func (p *Pruner) PruneJSONPayload(raw []byte) ([]byte, PruneStats, error) {
	stats := PruneStats{
		OriginalBytes: len(raw),
	}
	if len(raw) == 0 {
		stats.PrunedBytes = 0
		return raw, stats, nil
	}

	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return raw, stats, err
	}

	modified := false

	// Handle Anthropic top-level system prompt
	if sys, ok := root["system"]; ok {
		switch v := sys.(type) {
		case string:
			pruned, saved := p.PruneText(v)
			if saved > 0 {
				root["system"] = pruned
				modified = true
			}
		case []interface{}:
			for i, block := range v {
				if bMap, ok := block.(map[string]interface{}); ok {
					if bMap["type"] == "text" {
						if textStr, ok := bMap["text"].(string); ok {
							pruned, saved := p.PruneText(textStr)
							if saved > 0 {
								bMap["text"] = pruned
								v[i] = bMap
								modified = true
							}
						}
					}
				}
			}
		}
	}

	// Handle messages array (both OpenAI and Anthropic)
	if msgs, ok := root["messages"].([]interface{}); ok {
		for i, m := range msgs {
			msgMap, ok := m.(map[string]interface{})
			if !ok {
				continue
			}

			content, hasContent := msgMap["content"]
			if !hasContent {
				continue
			}

			switch c := content.(type) {
			case string:
				pruned, saved := p.PruneText(c)
				if saved > 0 {
					msgMap["content"] = pruned
					msgs[i] = msgMap
					modified = true
				}
			case []interface{}:
				for j, part := range c {
					if partMap, ok := part.(map[string]interface{}); ok {
						if partMap["type"] == "text" {
							if textStr, ok := partMap["text"].(string); ok {
								pruned, saved := p.PruneText(textStr)
								if saved > 0 {
									partMap["text"] = pruned
									c[j] = partMap
									modified = true
								}
							}
						}
					}
				}
			}
		}
	}

	if !modified {
		stats.PrunedBytes = len(raw)
		stats.SavedBytes = 0
		stats.ReductionRatio = 0.0
		return raw, stats, nil
	}

	prunedJSON, err := json.Marshal(root)
	if err != nil {
		return raw, stats, err
	}

	stats.PrunedBytes = len(prunedJSON)
	stats.SavedBytes = stats.OriginalBytes - stats.PrunedBytes
	if stats.OriginalBytes > 0 && stats.SavedBytes > 0 {
		stats.ReductionRatio = math.Round((float64(stats.SavedBytes)/float64(stats.OriginalBytes))*1000) / 1000
	}

	return prunedJSON, stats, nil
}
