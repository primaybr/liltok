package gemini

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/primaybr/liltok/internal/provider"
)

func TestGeminiAdapterSendChat(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"candidates": [{
				"content": {
					"parts": [{"text": "gemini response text"}],
					"role": "model"
				},
				"finishReason": "STOP"
			}],
			"usageMetadata": {
				"promptTokenCount": 30,
				"candidatesTokenCount": 15,
				"totalTokenCount": 45
			}
		}`))
	}))
	defer mockServer.Close()

	adapter := NewAdapter(mockServer.URL, "gem-key")

	req := &provider.UnifiedChatRequest{
		Model: "gemini-1.5-flash",
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "hello gemini"},
		},
	}

	resp, err := adapter.SendChat(context.Background(), req)
	if err != nil {
		t.Fatalf("send chat failed: %v", err)
	}

	if resp.Content != "gemini response text" {
		t.Errorf("expected gemini response text, got %s", resp.Content)
	}
	if resp.Usage.TotalTokens != 45 {
		t.Errorf("expected 45 total tokens, got %d", resp.Usage.TotalTokens)
	}
}

func TestGeminiMultiKeyFailover(t *testing.T) {
	key1Hit := false
	key2Hit := false

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k := r.URL.Query().Get("key")
		if k == "key-1" {
			key1Hit = true
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error": "RESOURCE_EXHAUSTED"}`))
			return
		}
		if k == "key-2" {
			key2Hit = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"candidates": [{
					"content": {"parts": [{"text": "recovered on key-2"}], "role": "model"},
					"finishReason": "STOP"
				}],
				"usageMetadata": {"promptTokenCount": 10, "candidatesTokenCount": 5, "totalTokenCount": 15}
			}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer mockServer.Close()

	adapter := NewAdapter(mockServer.URL, "key-1, key-2")
	if adapter.KeyCount() != 2 {
		t.Fatalf("expected 2 keys, got %d", adapter.KeyCount())
	}

	req := &provider.UnifiedChatRequest{
		Model: "gemini-1.5-flash",
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "test failover"},
		},
	}

	resp, err := adapter.SendChat(context.Background(), req)
	if err != nil {
		t.Fatalf("expected failover to succeed, got error: %v", err)
	}
	if resp.Content != "recovered on key-2" {
		t.Errorf("expected 'recovered on key-2', got: %q", resp.Content)
	}
	if !key1Hit || !key2Hit {
		t.Errorf("expected both key1 and key2 to be hit, key1=%v, key2=%v", key1Hit, key2Hit)
	}
}

func TestGeminiMultiKey401Failover(t *testing.T) {
	key1Hit := false
	key2Hit := false

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k := r.URL.Query().Get("key")
		if k == "disabled-key" {
			key1Hit = true
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{
				"error": {
					"code": 401,
					"message": "The bound service account is deleted or disabled.",
					"status": "UNAUTHENTICATED",
					"details": [{"reason": "ACCOUNT_STATE_INVALID"}]
				}
			}`))
			return
		}
		if k == "healthy-key" {
			key2Hit = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"candidates": [{
					"content": {"parts": [{"text": "recovered on healthy-key"}], "role": "model"},
					"finishReason": "STOP"
				}],
				"usageMetadata": {"promptTokenCount": 10, "candidatesTokenCount": 5, "totalTokenCount": 15}
			}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer mockServer.Close()

	adapter := NewAdapter(mockServer.URL, "disabled-key, healthy-key")
	req := &provider.UnifiedChatRequest{
		Model: "gemini-1.5-flash",
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "test 401 failover"},
		},
	}

	resp, err := adapter.SendChat(context.Background(), req)
	if err != nil {
		t.Fatalf("expected 401 failover to succeed, got error: %v", err)
	}
	if resp.Content != "recovered on healthy-key" {
		t.Errorf("expected 'recovered on healthy-key', got: %q", resp.Content)
	}
	if !key1Hit || !key2Hit {
		t.Errorf("expected both disabled-key and healthy-key to be hit, key1=%v, key2=%v", key1Hit, key2Hit)
	}
}

func TestGeminiCheckHealth(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1beta/models" && r.URL.Query().Get("key") == "valid-key" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"models": []}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error": "API_KEY_INVALID"}`))
	}))
	defer mockServer.Close()

	// 1. Unconfigured
	a0 := NewAdapter(mockServer.URL, "")
	if ok, err := a0.CheckHealth(context.Background()); ok || err == nil {
		t.Errorf("expected unconfigured adapter health to fail")
	}

	// 2. Valid key
	a1 := NewAdapter(mockServer.URL, "valid-key")
	if ok, err := a1.CheckHealth(context.Background()); !ok || err != nil {
		t.Errorf("expected valid key to pass health check: %v", err)
	}

	// 3. Invalid key
	a2 := NewAdapter(mockServer.URL, "invalid-key")
	if ok, err := a2.CheckHealth(context.Background()); ok || err == nil {
		t.Errorf("expected invalid key to fail health check")
	}

	// 4. Mixed invalid and valid keys
	a3 := NewAdapter(mockServer.URL, "invalid-key, valid-key")
	if ok, err := a3.CheckHealth(context.Background()); !ok || err != nil {
		t.Errorf("expected mixed keys with at least one valid key to pass health check: %v", err)
	}
}

func TestGeminiFunctionCalling(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"candidates": [{
				"content": {
					"parts": [{
						"functionCall": {
							"name": "SendMessage",
							"args": {
								"to": "a139b43b450f80250",
								"message": "Continue"
							}
						}
					}],
					"role": "model"
				},
				"finishReason": "STOP"
			}],
			"usageMetadata": {
				"promptTokenCount": 50,
				"candidatesTokenCount": 20,
				"totalTokenCount": 70
			}
		}`))
	}))
	defer mockServer.Close()

	adapter := NewAdapter(mockServer.URL, "gem-key")

	req := &provider.UnifiedChatRequest{
		Model: "gemini-flash-latest",
		Messages: []provider.UnifiedChatMessage{
			{Role: "user", Content: "Resume agent"},
		},
		Tools: []interface{}{
			map[string]interface{}{
				"name":        "SendMessage",
				"description": "Send a message to another agent",
				"input_schema": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"to":      map[string]interface{}{"type": "string"},
						"message": map[string]interface{}{"type": "string"},
					},
					"required": []string{"to", "message"},
				},
			},
		},
	}

	resp, err := adapter.SendChat(context.Background(), req)
	if err != nil {
		t.Fatalf("send chat with gemini tools failed: %v", err)
	}

	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Function.Name != "SendMessage" {
		t.Errorf("expected function SendMessage, got %s", resp.ToolCalls[0].Function.Name)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("expected finish_reason tool_calls, got %s", resp.FinishReason)
	}
}

func TestCleanGeminiSchema(t *testing.T) {
	rawSchema := map[string]interface{}{
		"$schema":              "http://json-schema.org/draft-07/schema#",
		"title":                "ComplexAgentTool",
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"action": map[string]interface{}{
				"const": "execute",
			},
			"limit": map[string]interface{}{
				"type":             "integer",
				"exclusiveMinimum": 0,
				"exclusiveMaximum": 100,
			},
			"items_list": map[string]interface{}{
				"type": "array",
				"prefixItems": []interface{}{
					map[string]interface{}{
						"type":                 "string",
						"additionalProperties": false,
						"pattern":              "^[a-z]+$",
					},
				},
			},
			"optional_val": map[string]interface{}{
				"anyOf": []interface{}{
					map[string]interface{}{"type": "string"},
					map[string]interface{}{"type": "null"},
				},
				"propertyNames": map[string]interface{}{
					"pattern": "^[a-z]+$",
				},
			},
		},
		"required": []interface{}{"action", "limit"},
	}

	cleaned := cleanGeminiSchema(rawSchema).(map[string]interface{})

	// Verify top-level forbidden keys are stripped
	if _, exists := cleaned["$schema"]; exists {
		t.Errorf("expected $schema to be removed")
	}
	if _, exists := cleaned["title"]; exists {
		t.Errorf("expected title to be removed")
	}
	if _, exists := cleaned["additionalProperties"]; exists {
		t.Errorf("expected additionalProperties to be removed")
	}

	// Verify properties are cleaned
	props, ok := cleaned["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected properties to be map")
	}

	// Verify const -> enum
	action := props["action"].(map[string]interface{})
	if _, exists := action["const"]; exists {
		t.Errorf("expected const to be stripped")
	}
	enumVal, ok := action["enum"].([]string)
	if !ok || len(enumVal) != 1 || enumVal[0] != "execute" {
		t.Errorf("expected enum: ['execute'], got %v", action["enum"])
	}

	// Verify exclusiveMinimum/exclusiveMaximum -> minimum/maximum
	limit := props["limit"].(map[string]interface{})
	if _, exists := limit["exclusiveMinimum"]; exists {
		t.Errorf("expected exclusiveMinimum to be stripped")
	}
	if limit["minimum"] != 0 {
		t.Errorf("expected minimum: 0, got %v", limit["minimum"])
	}
	if limit["maximum"] != 100 {
		t.Errorf("expected maximum: 100, got %v", limit["maximum"])
	}

	// Verify prefixItems -> items
	itemsList := props["items_list"].(map[string]interface{})
	if _, exists := itemsList["prefixItems"]; exists {
		t.Errorf("expected prefixItems to be stripped")
	}
	itemsMap, ok := itemsList["items"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected items to be map")
	}
	if itemsMap["type"] != "string" {
		t.Errorf("expected items.type string, got %v", itemsMap["type"])
	}
	if _, exists := itemsMap["additionalProperties"]; exists {
		t.Errorf("expected nested additionalProperties to be removed")
	}
	if _, exists := itemsMap["pattern"]; exists {
		t.Errorf("expected nested pattern to be removed")
	}

	// Verify nullable anyOf flattening
	optVal := props["optional_val"].(map[string]interface{})
	if optVal["type"] != "string" {
		t.Errorf("expected flattened type string, got %v", optVal["type"])
	}
	if optVal["nullable"] != true {
		t.Errorf("expected nullable true, got %v", optVal["nullable"])
	}
	if _, exists := optVal["propertyNames"]; exists {
		t.Errorf("expected propertyNames to be removed")
	}
}

// TestCleanGeminiSchemaArrayItems reproduces the exact INVALID_ARGUMENT errors seen in production:
// "properties[batch].items: missing field" and "properties[items].items: missing field"
func TestCleanGeminiSchemaArrayItems(t *testing.T) {
	// Case 1: array property with empty object as items (gets stripped to {}, causing "missing field")
	schema1 := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"batch": map[string]interface{}{
				"type":  "array",
				"items": map[string]interface{}{}, // empty - previously got stripped
			},
		},
	}
	out1 := cleanGeminiSchema(schema1).(map[string]interface{})
	props1 := out1["properties"].(map[string]interface{})
	batch := props1["batch"].(map[string]interface{})
	if batch["type"] != "array" {
		t.Errorf("expected batch.type=array, got %v", batch["type"])
	}
	itemsVal1, hasItems1 := batch["items"]
	if !hasItems1 {
		t.Fatal("expected batch.items to be injected but it was missing")
	}
	if m, ok := itemsVal1.(map[string]interface{}); !ok || m["type"] != "string" {
		t.Errorf("expected batch.items={type:string}, got %v", itemsVal1)
	}

	// Case 2: array property with NO items key at all
	schema2 := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"batch": map[string]interface{}{
				"type": "array",
				// no items key
			},
		},
	}
	out2 := cleanGeminiSchema(schema2).(map[string]interface{})
	props2 := out2["properties"].(map[string]interface{})
	batch2 := props2["batch"].(map[string]interface{})
	itemsVal2, hasItems2 := batch2["items"]
	if !hasItems2 {
		t.Fatal("expected default items to be injected for array with no items key")
	}
	if m, ok := itemsVal2.(map[string]interface{}); !ok || m["type"] != "string" {
		t.Errorf("expected injected items={type:string}, got %v", itemsVal2)
	}

	// Case 3: property named "items" of type array with empty nested items (the other production failure)
	schema3 := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"items": map[string]interface{}{
				"type":  "array",
				"items": map[string]interface{}{}, // empty
			},
		},
	}
	out3 := cleanGeminiSchema(schema3).(map[string]interface{})
	props3 := out3["properties"].(map[string]interface{})
	itemsProp := props3["items"].(map[string]interface{})
	nestedItems, hasNested := itemsProp["items"]
	if !hasNested {
		t.Fatal("expected items.items to be present")
	}
	if m, ok := nestedItems.(map[string]interface{}); !ok || m["type"] != "string" {
		t.Errorf("expected items.items={type:string}, got %v", nestedItems)
	}

	// Case 4: boolean items schema (true/false - JSON Schema draft-2020)
	schema4 := map[string]interface{}{
		"type":  "array",
		"items": true,
	}
	out4 := cleanGeminiSchema(schema4).(map[string]interface{})
	itemsVal4, hasItems4 := out4["items"]
	if !hasItems4 {
		t.Fatal("expected boolean items to be converted to {type:string}")
	}
	if m, ok := itemsVal4.(map[string]interface{}); !ok || m["type"] != "string" {
		t.Errorf("expected items={type:string} from boolean, got %v", itemsVal4)
	}
}
