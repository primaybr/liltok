package router

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/provider"
)

// scriptedProvider answers SendChat from a queue and records every request it receives.
type scriptedProvider struct {
	name  string
	tier  provider.ProviderTier
	mu    sync.Mutex
	queue []*provider.UnifiedChatResponse
	got   []*provider.UnifiedChatRequest
}

func (p *scriptedProvider) Name() string                                  { return p.name }
func (p *scriptedProvider) Tier() provider.ProviderTier                   { return p.tier }
func (p *scriptedProvider) CheckHealth(ctx context.Context) (bool, error) { return true, nil }

func (p *scriptedProvider) SendChat(ctx context.Context, req *provider.UnifiedChatRequest) (*provider.UnifiedChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = append(p.got, req)
	if len(p.queue) == 0 {
		return nil, errors.New("scriptedProvider: no reply queued")
	}
	r := *p.queue[0]
	p.queue = p.queue[1:]
	return &r, nil
}

func (p *scriptedProvider) StreamChat(ctx context.Context, req *provider.UnifiedChatRequest) (<-chan provider.UnifiedSSEEvent, <-chan error, error) {
	return nil, nil, errors.New("scriptedProvider: streaming is not scripted")
}

func newScripted(name string, replies ...*provider.UnifiedChatResponse) *scriptedProvider {
	return &scriptedProvider{name: name, tier: provider.TierFree, queue: replies}
}

// verifyRouter routes the request through the scripted providers in order.
func verifyRouter(t *testing.T, mutate func(*config.Config), scripted ...*scriptedProvider) *Router {
	t.Helper()
	cfg := config.DefaultConfig()
	if mutate != nil {
		mutate(cfg)
	}
	r := NewRouter(cfg)
	targets := make([]TargetSpec, 0, len(scripted))
	for _, sp := range scripted {
		r.SetProvider(sp.name, sp)
		targets = append(targets, TargetSpec{ProviderName: sp.name, UpstreamModel: sp.name + "-model"})
	}
	r.SetRoute(Route{ID: "verify", Strategy: StrategyFallback, Targets: targets})
	return r
}

func unverifiedRequest() *provider.UnifiedChatRequest {
	return &provider.UnifiedChatRequest{Model: "claude-sonnet-5", Tools: verifyTools(), Messages: []provider.UnifiedChatMessage{
		verifyUser("fix the gate"),
		verifyAssistant(verifyCall("r1", "Read", `{"file_path":"gate.go"}`)),
		verifyResult("r1", "package share"),
	}}
}

func failedBuildRequest() *provider.UnifiedChatRequest {
	return &provider.UnifiedChatRequest{Model: "claude-sonnet-5", Tools: verifyTools(), Messages: []provider.UnifiedChatMessage{
		verifyUser("fix the gate"),
		verifyAssistant(verifyShell("b1", "go build ./...")),
		verifyResult("b1", "Exit code 1\nFAIL example"),
	}}
}

func claimReply() *provider.UnifiedChatResponse {
	return &provider.UnifiedChatResponse{Model: "m", Role: "assistant", Content: claimText, FinishReason: "stop"}
}

func verifyToolReply() *provider.UnifiedChatResponse {
	return &provider.UnifiedChatResponse{
		Model: "m", Role: "assistant", Content: "Running the tests.", FinishReason: "tool_calls",
		ToolCalls: []provider.UnifiedToolCall{verifyShell("n1", "go test ./...")},
	}
}

func TestVerifyReminderGetsAToolCall(t *testing.T) {
	p1 := newScripted("vp1", claimReply(), verifyToolReply())
	p2 := newScripted("vp2", claimReply())
	r := verifyRouter(t, nil, p1, p2)

	resp, winner, err := r.DispatchChat(context.Background(), unverifiedRequest(), "verify")
	if err != nil || winner != "vp1" {
		t.Fatalf("the same target must be reminded first: winner %q err %v", winner, err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Function.Name != "Bash" {
		t.Fatalf("the reminder's tool call must replace the false claim: %+v", resp)
	}
	if strings.Contains(resp.Content, "build passes") {
		t.Errorf("the false claim must not reach the client: %q", resp.Content)
	}
	if len(p1.got) != 2 || len(p2.got) != 0 {
		t.Fatalf("requests: vp1 %d (want 2), vp2 %d (want 0)", len(p1.got), len(p2.got))
	}
	msgs := p1.got[1].Messages
	n := len(msgs)
	if n < 2 || msgs[n-2].Role != "assistant" || msgs[n-2].Content != claimText ||
		msgs[n-1].Role != "user" || !strings.Contains(msgs[n-1].Content, "no build or test command has run") ||
		p1.got[1].RawPayload != nil {
		t.Fatalf("reminder request must end with the claim and the reminder, built from Messages: %+v", msgs)
	}
	if counts := r.UnverifiedClaimCounts(); len(counts) != 1 || counts[0].Model != "vp1-model" || counts[0].Verdict != "unverified" || counts[0].Count != 1 {
		t.Errorf("counts = %+v, want one unverified detection for vp1-model", counts)
	}
}

func TestVerifyIgnoredReminderFailsOverToNextTarget(t *testing.T) {
	p1 := newScripted("vp1", claimReply(), claimReply())
	p2 := newScripted("vp2", verifyToolReply())
	r := verifyRouter(t, nil, p1, p2)
	req := unverifiedRequest()
	original := len(req.Messages)

	resp, winner, err := r.DispatchChat(context.Background(), req, "verify")
	if err != nil || winner != "vp2" || len(resp.ToolCalls) != 1 {
		t.Fatalf("a target that ignores the reminder must be failed over: winner %q err %v resp %+v", winner, err, resp)
	}
	if len(p2.got) != 1 || len(p2.got[0].Messages) != original {
		t.Fatalf("the next target gets the original request, without the claim or reminder: %d messages, want %d", len(p2.got[0].Messages), original)
	}
}

func TestVerifyExhaustedAppendsNotice(t *testing.T) {
	p1 := newScripted("vp1", claimReply(), claimReply())
	p2 := newScripted("vp2", claimReply(), claimReply())
	r := verifyRouter(t, nil, p1, p2)

	resp, winner, err := r.DispatchChat(context.Background(), unverifiedRequest(), "verify")
	if err != nil || winner != "vp2" {
		t.Fatalf("when nothing verifies, the last claiming reply is returned: winner %q err %v", winner, err)
	}
	if !strings.HasPrefix(resp.Content, claimText) || !strings.HasSuffix(resp.Content, "[liltok] unverified: no build or test ran this turn.") {
		t.Fatalf("reply must keep its text and end with the notice: %q", resp.Content)
	}
	if len(p1.got) != 2 || len(p2.got) != 2 {
		t.Errorf("each target is tried once and reminded once: vp1 %d, vp2 %d", len(p1.got), len(p2.got))
	}
}

func TestVerifyContradictedNotice(t *testing.T) {
	p1 := newScripted("vp1", claimReply(), claimReply())
	r := verifyRouter(t, nil, p1)

	resp, _, err := r.DispatchChat(context.Background(), failedBuildRequest(), "verify")
	if err != nil || !strings.HasSuffix(resp.Content, "[liltok] unverified: the last build or test run failed.") {
		t.Fatalf("a claim after a failing run gets the contradicted notice: %v %q", err, resp.Content)
	}
	if counts := r.UnverifiedClaimCounts(); len(counts) == 0 || counts[0].Verdict != "contradicted" {
		t.Errorf("counts = %+v, want a contradicted detection", counts)
	}
}

// A false claim is not a provider fault, so the circuit breaker must never be charged for one.
func TestVerifyClaimDoesNotChargeTheBreaker(t *testing.T) {
	replies := make([]*provider.UnifiedChatResponse, 0, 20)
	for i := 0; i < 20; i++ {
		replies = append(replies, claimReply())
	}
	p1 := newScripted("vp1", replies...)
	r := verifyRouter(t, nil, p1)

	for i := 0; i < 8; i++ {
		if _, _, err := r.DispatchChat(context.Background(), unverifiedRequest(), "verify"); err != nil {
			t.Fatalf("dispatch %d: %v", i, err)
		}
	}
	cb, ok := r.GetBreaker("vp1/vp1-model")
	if !ok {
		t.Fatal("no breaker was created for vp1/vp1-model")
	}
	if state, failures := cb.State(); !cb.Allow() || failures != 0 {
		t.Fatalf("breaker state %v with %d failures after 8 false claims; want closed with none", state, failures)
	}
}

func TestVerifyPremiumTargetIsNotChecked(t *testing.T) {
	p1 := newScripted("vp1", claimReply())
	p1.tier = provider.TierPremium
	r := verifyRouter(t, nil, p1)

	resp, _, err := r.DispatchChat(context.Background(), unverifiedRequest(), "verify")
	if err != nil || resp.Content != claimText || len(p1.got) != 1 {
		t.Fatalf("a premium reply must pass through untouched: %v %q requests %d", err, resp.Content, len(p1.got))
	}
}

func TestVerifyDisabledByConfig(t *testing.T) {
	p1 := newScripted("vp1", claimReply())
	r := verifyRouter(t, func(c *config.Config) { c.Routes.VerifyClaims = false }, p1)

	resp, _, err := r.DispatchChat(context.Background(), unverifiedRequest(), "verify")
	if err != nil || resp.Content != claimText || len(p1.got) != 1 {
		t.Fatalf("routes.verify_claims: false must leave replies alone: %v %q requests %d", err, resp.Content, len(p1.got))
	}
}

func TestVerifyNoticeAfterThinkBlock(t *testing.T) {
	reply := claimReply()
	reply.Content = "<think>checking the plan</think>" + claimText
	p1 := newScripted("vp1", reply, reply)
	r := verifyRouter(t, nil, p1)

	resp, _, err := r.DispatchChat(context.Background(), unverifiedRequest(), "verify")
	if err != nil || !strings.HasPrefix(resp.Content, "<think>checking the plan</think>") || !strings.HasSuffix(resp.Content, "no build or test ran this turn.") {
		t.Fatalf("the notice goes after the whole reply, think block included: %v %q", err, resp.Content)
	}
}

func TestRecordUnverifiedClaimIsConcurrencySafe(t *testing.T) {
	r := NewRouter(config.DefaultConfig())
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.RecordUnverifiedClaim("m-a", "unverified")
			r.RecordUnverifiedClaim("m-b", "contradicted")
		}()
	}
	wg.Wait()
	counts := r.UnverifiedClaimCounts()
	if len(counts) != 2 || counts[0].Model != "m-a" || counts[0].Count != 50 || counts[1].Model != "m-b" || counts[1].Count != 50 {
		t.Fatalf("counts = %+v, want m-a 50 and m-b 50 in that order", counts)
	}
}

// Plan mode ends its plan with expected outcomes ("all tests pass") that are not reports, and the
// translator writes the reply text into the plan file, so the check must leave plan turns alone.
func TestVerifySkippedInPlanMode(t *testing.T) {
	plan := claimReply()
	plan.Content = "Step 1: change the gate.\n\n- go vet is clean\n- all tests pass"
	p1 := newScripted("vp1", plan)
	r := verifyRouter(t, nil, p1)
	req := &provider.UnifiedChatRequest{Model: "claude-sonnet-5", Tools: verifyTools(), Messages: []provider.UnifiedChatMessage{
		verifyUser("plan the gate change"),
		verifyUser("<system-reminder>\nPlan mode is active. You should create your plan at /plans/gate.md using the Write tool.\n</system-reminder>"),
	}}

	resp, _, err := r.DispatchChat(context.Background(), req, "verify")
	if err != nil || resp.Content != plan.Content || len(p1.got) != 1 {
		t.Fatalf("a plan-mode reply must pass through untouched: %v %q requests %d", err, resp.Content, len(p1.got))
	}
}
