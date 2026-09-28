package router

import (
	"strings"
	"testing"

	"github.com/primaybr/liltok/internal/provider"
)

// symbolNoise mimics a reply that degenerated: punctuation, short digit runs and blank lines,
// with no word in it.
func symbolNoise(repeats int) string {
	return strings.Repeat("\"\":11......:/....\n\n\n/\n\n:\n\n?\n\n, (\n\n - -\n\n?", repeats)
}

func TestDegenerateCut(t *testing.T) {
	prose := "The gate now rejects a loopback URL with userinfo and a private host with a port."
	table := "| Rule | Result |\n|------|--------|\n| url | reject |\n| pii | pass |\n\nBoth rules ran."
	cases := []struct {
		name string
		text string
		bad  bool
		cut  int
	}{
		{"plain prose", prose, false, 0},
		{"prose then symbol noise", prose + " `secretKey:" + symbolNoise(8), true, len(prose + " `secretKey")},
		{"markdown table", table, false, 0},
		{"banner line", strings.Repeat("=", 66) + "\n Share Scan\n" + strings.Repeat("=", 66), false, 0},
		// The cut lands after the last word, so the closing period goes with the run.
		{"blank lines only after prose", prose + strings.Repeat("\n", maxWordlessBytes), true, len(prose) - 1},
		{"cjk prose", strings.Repeat("缓存命中率提高了，延迟下降了。", 20), false, 0},
		{"numeric list", "Ports in use: " + strings.Repeat("8080, 5432, 6379, ", 20), false, 0},
		{"regex answer", "Use this pattern:\n" + strings.Repeat(`\.(?:1[6-9]|2\d|3[01])\.(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)`, 4), false, 0},
		{"one repeated symbol", prose + " " + strings.Repeat("!", 200), true, len(prose) - 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cut, bad := degenerateCut(tc.text)
			if bad != tc.bad || (bad && cut != tc.cut) {
				t.Fatalf("degenerateCut = (%d, %v), want (%d, %v)", cut, bad, tc.cut, tc.bad)
			}
		})
	}
}

func TestCheckReplyRejectsDegenerateText(t *testing.T) {
	req := &provider.UnifiedChatRequest{Messages: []provider.UnifiedChatMessage{{Role: "user", Content: "summarize"}}}
	target := TargetSpec{ProviderName: "p", UpstreamModel: "m"}
	resp := &provider.UnifiedChatResponse{Content: "Summary of the fixes. " + symbolNoise(8), FinishReason: "stop"}
	if err := checkReply(req, target, resp); err == nil || !strings.Contains(err.Error(), errDegenerateReply.Error()) {
		t.Fatalf("a degenerate reply must fail over, got %v", err)
	}
	resp = &provider.UnifiedChatResponse{Content: "Summary of the fixes.", FinishReason: "stop"}
	if err := checkReply(req, target, resp); err != nil {
		t.Fatalf("a plain reply must pass, got %v", err)
	}
}

func TestLiveStreamDegenerateBeforeCommitFailsOver(t *testing.T) {
	body := longText(600)
	scripts := map[string]streamScript{
		"m1": {events: textEvents("Short start. ", symbolNoise(8))},
		"m2": {events: textEvents(body[:450], body[450:])},
	}
	r, sp := liveRouter(t, scripts, "m1", "m2")
	sink := &recordingSink{}
	resp, _, err := liveDispatch(r, sink, nil)
	if err != nil || resp.Content != body || sp.streamed["m2"] != 1 {
		t.Fatalf("degeneration before commit must fail over: err %v streamed %v", err, sp.streamed)
	}
	if strings.Contains(sink.text.String(), "(") {
		t.Fatal("no noise from the failed attempt may reach the client")
	}
}

func TestLiveStreamDegenerateAfterCommitCutsTheReply(t *testing.T) {
	body := longText(450)
	scripts := map[string]streamScript{
		"m1": {events: textEvents(body, symbolNoise(2), symbolNoise(2), symbolNoise(4))},
		"m2": {events: textEvents("unused")},
	}
	r, sp := liveRouter(t, scripts, "m1", "m2")
	sink := &recordingSink{}
	_, _, err := liveDispatch(r, sink, nil)
	if !IsCommittedStreamError(err) || sp.streamed["m2"] != 0 {
		t.Fatalf("degeneration after commit must end the turn without failover: err %v streamed %v", err, sp.streamed)
	}
	if sink.failed != nil || sink.finished == nil {
		t.Fatalf("the reply must be finished at the cut, not failed: failed %v", sink.failed)
	}
	got := sink.finished.Content
	if strings.Contains(got, "(") || strings.Contains(sink.text.String(), "(") || !strings.HasPrefix(body, got) || len(got) < len(body)-liveHoldback {
		t.Fatalf("the client must get the prose up to the noise and none of the noise, got %d of %d bytes", len(got), len(body))
	}
	if sink.finished.FinishReason != "stop" {
		t.Fatalf("finish reason %q, want stop", sink.finished.FinishReason)
	}
}
