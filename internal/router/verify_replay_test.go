package router

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/primaybr/liltok/internal/provider"
)

// replyFromAnthropicJSON rebuilds the parts of a stored Anthropic reply that assessClaim reads:
// the text and whether it made tool calls.
func replyFromAnthropicJSON(payload []byte) *provider.UnifiedChatResponse {
	if len(payload) > 2 && payload[0] == 0x1f && payload[1] == 0x8b {
		if zr, err := gzip.NewReader(bytes.NewReader(payload)); err == nil {
			if plain, err := io.ReadAll(zr); err == nil {
				payload = plain
			}
		}
	}
	var msg struct {
		Content []map[string]interface{} `json:"content"`
	}
	if json.Unmarshal(payload, &msg) != nil {
		return nil
	}
	resp := &provider.UnifiedChatResponse{}
	for _, b := range msg.Content {
		switch b["type"] {
		case "text":
			s, _ := b["text"].(string)
			resp.Content += s
		case "tool_use":
			c := provider.UnifiedToolCall{Type: "function"}
			c.Function.Name, _ = b["name"].(string)
			resp.ToolCalls = append(resp.ToolCalls, c)
		}
	}
	return resp
}

func isAgentConversation(req *provider.UnifiedChatRequest) bool {
	for _, m := range req.Messages {
		if m.Role == "tool" || len(m.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// TestReplayVerifyClaims runs the false-success check over every agent conversation in a copy of a
// cache database and logs each flagged reply's closing text, so a person can read them. It is the
// release gate for the check: run it after any change to the claim phrases or the window rule.
//
//	sqlite3 -readonly <live db> ".backup '<scratch dir>/replay.db'"
//	LILTOK_REPLAY_DB=<scratch dir>/replay.db go test ./internal/router -run TestReplayVerifyClaims -v -count=1
//
// The cache stores the model the client asked for, not the model that answered, and the canonical
// request may omit the tools list, so a conversation with any tool call or tool result counts as an
// agent turn and is given a stand-in tools list.
func TestReplayVerifyClaims(t *testing.T) {
	path := os.Getenv("LILTOK_REPLAY_DB")
	if path == "" {
		t.Skip("set LILTOK_REPLAY_DB to a copy of a cache database to run the replay")
	}
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	rows, err := conn.Query(`SELECT hash, model, normalized_prompt, response_payload FROM cache_entries`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var scanned, agent, finals, claims, flagged int
	verdicts := map[string]int{}
	for rows.Next() {
		var hash, model, prompt string
		var payload []byte
		if rows.Scan(&hash, &model, &prompt, &payload) != nil {
			continue
		}
		scanned++
		req, err := provider.ParseUnifiedRequest([]byte(prompt), true)
		if err != nil || !isAgentConversation(req) {
			continue
		}
		agent++
		req.Tools = []interface{}{map[string]interface{}{"name": "Bash"}}
		resp := replyFromAnthropicJSON(payload)
		if resp == nil {
			continue
		}
		if len(resp.ToolCalls) == 0 {
			finals++
			if claimsSuccess(lastParagraph(visibleReplyText(resp))) {
				claims++
			}
		}
		v := assessClaim(req, resp, nil)
		if v == claimNone {
			continue
		}
		flagged++
		verdicts[v.String()]++
		tail := lastParagraph(visibleReplyText(resp))
		if len(tail) > 240 {
			tail = tail[len(tail)-240:]
		}
		t.Logf("FLAGGED %s %s %s: %q", hash[:min(8, len(hash))], model, v, tail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("scanned %d cache entries, %d agent conversations, %d final replies, %d with a success phrase, %d flagged (%v)", scanned, agent, finals, claims, flagged, verdicts)
}
