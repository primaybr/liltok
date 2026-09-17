package semantic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// Embedder generates vector embeddings for text queries.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Dimension() int
}

// FastLocalEmbedder is a pure-Go, zero-dependency embedder using subword n-gram hashing
// and L2 projection. It operates in sub-millisecond time without external servers.
type FastLocalEmbedder struct {
	dimension int
}

// NewFastLocalEmbedder creates a new FastLocalEmbedder with specified dimension (default 256).
func NewFastLocalEmbedder(dimension int) *FastLocalEmbedder {
	if dimension <= 0 {
		dimension = 256
	}
	return &FastLocalEmbedder{dimension: dimension}
}

// Dimension returns vector length.
func (e *FastLocalEmbedder) Dimension() int {
	return e.dimension
}

// Embed generates an L2-normalized feature vector from text.
func (e *FastLocalEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	vec := make([]float32, e.dimension)
	normalized := strings.ToLower(strings.TrimSpace(text))
	if len(normalized) == 0 {
		return vec, nil
	}

	// 1. Tokenize into words
	words := strings.FieldsFunc(normalized, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})

	stopWords := map[string]bool{
		"the": true, "a": true, "an": true, "in": true, "on": true, "at": true,
		"to": true, "for": true, "of": true, "is": true, "are": true, "and": true,
	}

	for _, w := range words {
		weight := float32(4.0)
		if stopWords[w] {
			weight = 0.5
		}
		// Full word feature (dominant weight)
		addHashedFeature(vec, w, weight, e.dimension)

		// Character prefixes/3-grams for subword similarity
		runes := []rune(w)
		if len(runes) >= 2 {
			addHashedFeature(vec, string(runes[:2]), 1.5, e.dimension)
		}
		if len(runes) >= 3 {
			for i := 0; i <= len(runes)-3; i++ {
				ngram := string(runes[i : i+3])
				addHashedFeature(vec, ngram, 0.5, e.dimension)
			}
		}
	}

	// 2. L2 Normalization
	var sumSq float64
	for _, v := range vec {
		sumSq += float64(v) * float64(v)
	}
	if sumSq > 0 {
		norm := float32(math.Sqrt(sumSq))
		for i := range vec {
			vec[i] /= norm
		}
	}

	return vec, nil
}

func addHashedFeature(vec []float32, feature string, weight float32, dim int) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(feature))
	hashVal := h.Sum64()

	idx := int(hashVal % uint64(dim))
	// Use bit 63 for sign (+1 or -1)
	if (hashVal & (1 << 63)) != 0 {
		vec[idx] += weight
	} else {
		vec[idx] -= weight
	}
}

// OllamaEmbedder connects to a local Ollama instance for dense vector embeddings.
type OllamaEmbedder struct {
	baseURL    string
	model      string
	httpClient *http.Client
	dimension  int
}

// NewOllamaEmbedder creates a new Ollama embedder.
func NewOllamaEmbedder(baseURL, model string, timeout time.Duration) *OllamaEmbedder {
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	if model == "" {
		model = "nomic-embed-text"
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &OllamaEmbedder{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		httpClient: &http.Client{
			Timeout: timeout,
		},
		dimension: 768,
	}
}

// Dimension returns the expected embedding vector dimension.
func (o *OllamaEmbedder) Dimension() int {
	return o.dimension
}

// Embed generates embeddings via Ollama's HTTP API.
func (o *OllamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	reqBody, _ := json.Marshal(map[string]string{
		"model":  o.model,
		"prompt": text,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/embeddings", bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama embed request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama embed error (status %d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode ollama embedding response: %w", err)
	}

	o.dimension = len(result.Embedding)

	// L2 Normalize
	var sumSq float64
	for _, v := range result.Embedding {
		sumSq += float64(v) * float64(v)
	}
	if sumSq > 0 {
		norm := float32(math.Sqrt(sumSq))
		for i := range result.Embedding {
			result.Embedding[i] /= norm
		}
	}

	return result.Embedding, nil
}
