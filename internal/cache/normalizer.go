package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// NormalizationOptions provides configuration flags for the normalizer.
type NormalizationOptions struct {
	CacheNonzeroTemperature bool
}

// NormalizedRequest carries the canonical representation and cacheability verdict.
type NormalizedRequest struct {
	Model          string
	Hash           string
	CanonicalJSON  string
	IsCacheable    bool
	BypassReason   string
}

// NormalizePayload canonicalizes incoming JSON payload into deterministic SHA-256 hash.
func NormalizePayload(rawJSON []byte, opts NormalizationOptions) (*NormalizedRequest, error) {
	var obj map[string]interface{}
	if err := json.Unmarshal(rawJSON, &obj); err != nil {
		return nil, fmt.Errorf("malformed json payload: %w", err)
	}

	model, _ := obj["model"].(string)
	if model == "" {
		model = "unknown-model"
	}

	// 1. Remove ephemeral transport parameters
	delete(obj, "stream")

	// 2. Check temperature and determinism
	isCacheable := true
	bypassReason := ""

	var temp float64
	hasTemp := false
	if tVal, exists := obj["temperature"]; exists {
		hasTemp = true
		if fVal, ok := tVal.(float64); ok {
			temp = math.Round(fVal*1000) / 1000
			obj["temperature"] = temp
		}
	} else {
		// Default temperature in OpenAI is typically 1.0, but if not specified we treat as standard
		temp = 0.0
	}

	_, hasSeed := obj["seed"]

	if hasTemp && temp > 0 && !hasSeed && !opts.CacheNonzeroTemperature {
		isCacheable = false
		bypassReason = "temperature > 0 without seed"
	}

	// 3. Normalize float parameters
	if topP, exists := obj["top_p"]; exists {
		if fVal, ok := topP.(float64); ok {
			obj["top_p"] = math.Round(fVal*1000) / 1000
		}
	}
	if presP, exists := obj["presence_penalty"]; exists {
		if fVal, ok := presP.(float64); ok {
			obj["presence_penalty"] = math.Round(fVal*1000) / 1000
		}
	}
	if freqP, exists := obj["frequency_penalty"]; exists {
		if fVal, ok := freqP.(float64); ok {
			obj["frequency_penalty"] = math.Round(fVal*1000) / 1000
		}
	}

	// 4. Sort tools deterministically by function name
	if tools, ok := obj["tools"].([]interface{}); ok && len(tools) > 1 {
		sort.SliceStable(tools, func(i, j int) bool {
			nameI := extractToolName(tools[i])
			nameJ := extractToolName(tools[j])
			return nameI < nameJ
		})
		obj["tools"] = tools
	}

	// 5. Canonicalize nested structures recursively
	canonicalData := canonicalizeValue(obj)

	// 6. Marshal to deterministic JSON (Go encoding/json naturally sorts map keys)
	canonicalBytes, err := json.Marshal(canonicalData)
	if err != nil {
		return nil, fmt.Errorf("canonical marshaling failed: %w", err)
	}

	// 7. Compute SHA-256 Hash: SHA256(Model + ":" + CanonicalJSON)
	hasher := sha256.New()
	hasher.Write([]byte(model))
	hasher.Write([]byte(":"))
	hasher.Write(canonicalBytes)
	hash := hex.EncodeToString(hasher.Sum(nil))

	return &NormalizedRequest{
		Model:         model,
		Hash:          hash,
		CanonicalJSON: string(canonicalBytes),
		IsCacheable:   isCacheable,
		BypassReason:  bypassReason,
	}, nil
}

func extractToolName(tool interface{}) string {
	m, ok := tool.(map[string]interface{})
	if !ok {
		return ""
	}
	if fn, ok := m["function"].(map[string]interface{}); ok {
		if name, ok := fn["name"].(string); ok {
			return name
		}
	}
	if name, ok := m["name"].(string); ok {
		return name
	}
	return ""
}

func canonicalizeValue(val interface{}) interface{} {
	switch v := val.(type) {
	case map[string]interface{}:
		cleanMap := make(map[string]interface{}, len(v))
		for k, child := range v {
			// Replace raw base64 image data with sha256 digest to keep hash compact
			if k == "url" {
				if sVal, ok := child.(string); ok && strings.HasPrefix(sVal, "data:image/") {
					digest := sha256.Sum256([]byte(sVal))
					cleanMap[k] = "sha256:" + hex.EncodeToString(digest[:])
					continue
				}
			}
			cleanMap[k] = canonicalizeValue(child)
		}
		return cleanMap
	case []interface{}:
		cleanSlice := make([]interface{}, len(v))
		for i, item := range v {
			cleanSlice[i] = canonicalizeValue(item)
		}
		return cleanSlice
	case string:
		// Normalize whitespace while preserving essential formatting
		return strings.TrimSpace(v)
	case float64:
		// Float precision rounding
		return math.Round(v*100000) / 100000
	default:
		return v
	}
}
