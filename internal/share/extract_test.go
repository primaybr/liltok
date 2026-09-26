package share_test

import (
	"encoding/json"
	"testing"

	"github.com/primaybr/liltok/internal/share"
)

func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func anthReq(t *testing.T, msgs ...map[string]interface{}) string {
	return mustJSON(t, map[string]interface{}{"model": "claude-sonnet-5", "max_tokens": 4096, "messages": msgs})
}

func user(content interface{}) map[string]interface{} {
	return map[string]interface{}{"role": "user", "content": content}
}

func assistant(text string) map[string]interface{} {
	return map[string]interface{}{"role": "assistant", "content": text}
}

func text(s string) map[string]interface{} { return map[string]interface{}{"type": "text", "text": s} }

const anthAnswer = `{"type":"message","content":[{"type":"text","text":"Use slices.Reverse."}],"stop_reason":"end_turn"}`

const question = "How do I reverse a slice in Go without allocating a new one?"

func TestExtract(t *testing.T) {
	cases := []struct {
		name, prompt, answer, wantQ, wantSkip string
	}{
		{"plain anthropic question", anthReq(t, user(question)), anthAnswer, question, ""},
		{"claude code first turn", anthReq(t, user([]interface{}{
			text("<system-reminder>\nAs you answer the user's questions, you can use the following context:\n# userEmail\nThe user's email address is someone@corp.example.\n</system-reminder>"),
			text(question),
		})), anthAnswer, question, ""},
		{"claude md inside reminder", anthReq(t, user("<system-reminder>Contents of CLAUDE.md: internal notes</system-reminder>\n"+question)), anthAnswer, question, ""},
		{"ide selection removed", anthReq(t, user("<ide_selection>func secret() {}</ide_selection>\n"+question)), anthAnswer, question, ""},
		{"unknown tag", anthReq(t, user("<custom-context>internal</custom-context> "+question)), anthAnswer, "", share.SkipUnknownTag},
		{"tool result turn", anthReq(t, user([]interface{}{map[string]interface{}{"type": "tool_result", "tool_use_id": "t1", "content": "ok"}})), anthAnswer, "", share.SkipToolTurn},
		{"image question", anthReq(t, user([]interface{}{map[string]interface{}{"type": "image", "source": map[string]string{"type": "base64", "data": "AA=="}}})), anthAnswer, "", share.SkipNonTextQuestion},
		{"short follow-up", anthReq(t, user(question), assistant("Use slices.Reverse."), user("and for maps?")), anthAnswer, "", share.SkipFollowUp},
		{"long standalone later turn", anthReq(t, user(question), assistant("Use slices.Reverse."),
			user("Separately, how do I sort a slice of structs by two fields in Go with slices.SortFunc?")), anthAnswer,
			"Separately, how do I sort a slice of structs by two fields in Go with slices.SortFunc?", ""},
		{"context reference", anthReq(t, user("Can you fix this function so it stops panicking on nil input?")), anthAnswer, "", share.SkipContextRef},
		{"ends with assistant turn", anthReq(t, user(question), assistant("Sure")), anthAnswer, "", share.SkipToolTurn},
		{"only reminder", anthReq(t, user("<system-reminder>context only</system-reminder>")), anthAnswer, "", share.SkipNoUserText},
		{"tool use answer", anthReq(t, user(question)), `{"content":[{"type":"tool_use","id":"t","name":"Bash","input":{}}],"stop_reason":"tool_use"}`, "", share.SkipNonTextAnswer},
		{"truncated answer", anthReq(t, user(question)), `{"content":[{"type":"text","text":"Use"}],"stop_reason":"max_tokens"}`, "", share.SkipNonTextAnswer},
		{"thinking plus text answer", anthReq(t, user(question)), `{"content":[{"type":"thinking","thinking":"..."},{"type":"text","text":"Use slices.Reverse."}],"stop_reason":"end_turn"}`, question, ""},
		{"openai question", mustJSON(t, map[string]interface{}{"model": "gpt-4o", "messages": []interface{}{
			map[string]string{"role": "system", "content": "You are helpful."}, map[string]string{"role": "user", "content": question}}}),
			`{"choices":[{"message":{"role":"assistant","content":"Use slices.Reverse."},"finish_reason":"stop"}]}`, question, ""},
		{"openai tool call answer", mustJSON(t, map[string]interface{}{"model": "gpt-4o", "messages": []interface{}{map[string]string{"role": "user", "content": question}}}),
			`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c"}]},"finish_reason":"tool_calls"}]}`, "", share.SkipNonTextAnswer},
		{"openai tool turn", mustJSON(t, map[string]interface{}{"model": "gpt-4o", "messages": []interface{}{
			map[string]string{"role": "user", "content": question}, map[string]string{"role": "tool", "content": "ok"}}}),
			`{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`, "", share.SkipToolTurn},
		{"not json", "not a request", anthAnswer, "", share.SkipNotRequest},
	}
	for _, tc := range cases {
		c, skip := share.Extract(share.ExtractInput{NormalizedPrompt: tc.prompt, ResponsePayload: tc.answer})
		if skip != tc.wantSkip || c.Question != tc.wantQ {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", tc.name, c.Question, skip, tc.wantQ, tc.wantSkip)
		}
		if skip == "" && c.ID != share.QuestionID(tc.wantQ) {
			t.Errorf("%s: ID = %q, want QuestionID of the question", tc.name, c.ID)
		}
	}
}

func TestQuestionIDNormalizes(t *testing.T) {
	if share.QuestionID("How  do I\nreverse a slice?") != share.QuestionID("how do i reverse a slice?") {
		t.Error("QuestionID must ignore case and whitespace runs")
	}
	if share.QuestionID("a b") == share.QuestionID("a c") {
		t.Error("different questions must get different IDs")
	}
}
