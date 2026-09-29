package router

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/primaybr/liltok/internal/provider"
	"github.com/primaybr/liltok/internal/telemetry"
)

// errUnverifiedClaim marks a final reply that says a build or test run passed when the conversation
// shows no such run, or a failed one. The model is not broken, so the circuit breaker is not charged.
var errUnverifiedClaim = errors.New("reported a passing build or test run that the conversation does not support")

// unverifiedClaimError carries the verdict so the caller can pick the notice text.
type unverifiedClaimError struct {
	verdict  claimVerdict
	provider string
	model    string
}

func (e *unverifiedClaimError) Error() string {
	return fmt.Sprintf("upstream provider %s model %s %v (%s)", e.provider, e.model, errUnverifiedClaim, e.verdict)
}

func (e *unverifiedClaimError) Unwrap() error { return errUnverifiedClaim }

// verifyReminderPrompt follows a false claim in the reminder request. It carries the same
// "[System Reminder]" prefix as continuationPrompt, which the OpenAI adapter and
// isAutomatedUserMessage both recognise as client-injected text.
const verifyReminderPrompt = "[System Reminder] You reported that the build or tests pass, but no build or test command has run " +
	"since the user's last message, or the last one failed. Run the build or tests now with a tool call, then report the " +
	"real result. Reply with the tool call only; do not repeat the message."

// unverifiedNotice is appended to a claiming reply that nothing could verify.
func unverifiedNotice(v claimVerdict) string {
	if v == claimContradicted {
		return "\n\n[liltok] unverified: the last build or test run failed."
	}
	return "\n\n[liltok] unverified: no build or test ran this turn."
}

// compileVerifyCommands compiles the routes.verify_commands patterns; one that does not compile is
// logged and skipped so a typo cannot stop the gateway.
func compileVerifyCommands(patterns []string) []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			telemetry.Log.Warn().Str("pattern", p).Err(err).Msg("Ignoring routes.verify_commands entry that is not a valid regular expression")
			continue
		}
		out = append(out, re)
	}
	return out
}

// verifyReply returns an *unverifiedClaimError for a final reply from a non-premium target that
// reports a passing build or test run the conversation does not support, and counts the detection.
func (r *Router) verifyReply(req *provider.UnifiedChatRequest, target TargetSpec, p provider.ProviderClient, resp *provider.UnifiedChatResponse) error {
	if !r.verifyClaims || p == nil || p.Tier() == provider.TierPremium {
		return nil
	}
	v := assessClaim(req, resp, r.verifyCommands)
	if v == claimNone {
		return nil
	}
	r.RecordUnverifiedClaim(target.UpstreamModel, v.String())
	return &unverifiedClaimError{verdict: v, provider: target.ProviderName, model: target.UpstreamModel}
}

// remindToVerify asks target once, with the false claim and verifyReminderPrompt appended to the
// conversation, for the missing build or test call. It returns the reply only when it passes
// checkReply and makes a tool call; nil means the target did not comply, could not be asked, or
// the longer request would not fit its context window.
func (r *Router) remindToVerify(ctx context.Context, req *provider.UnifiedChatRequest, claim *provider.UnifiedChatResponse, target TargetSpec, p provider.ProviderClient, approxTokens int) *provider.UnifiedChatResponse {
	claimBody := visibleReplyText(claim)
	if approxTokens+(len(claimBody)+len(verifyReminderPrompt))/4 > r.GetModelContextWindow(target.ProviderName, target.UpstreamModel) {
		return nil
	}
	remindReq := *req
	remindReq.RawPayload = nil // providers pass a raw OpenAI payload through; build from Messages instead
	remindReq.Stream = false
	remindReq.Model = target.UpstreamModel
	remindReq.Messages = append(append([]provider.UnifiedChatMessage(nil), req.Messages...),
		provider.UnifiedChatMessage{Role: "assistant", Content: claimBody},
		provider.UnifiedChatMessage{Role: "user", Content: verifyReminderPrompt},
	)

	attemptCtx, cancel := ctx, context.CancelFunc(func() {})
	if r.attemptTimeout > 0 && p.Tier() != provider.TierPremium {
		attemptCtx, cancel = context.WithTimeout(ctx, r.attemptTimeout)
	}
	start := time.Now()
	resp, err := p.SendChat(attemptCtx, &remindReq)
	cancel()
	raw := snapshotResponse(resp)
	if err == nil {
		err = checkReply(&remindReq, target, resp)
	}
	if err == nil && len(resp.ToolCalls) == 0 {
		err = errors.New("reply to the verification reminder made no tool call")
	}
	observeAttempt(ctx, AttemptResult{Provider: target.ProviderName, Model: target.UpstreamModel, Response: raw, Err: err, Latency: time.Since(start)})
	if err != nil {
		telemetry.Log.Warn().
			Str("provider", target.ProviderName).
			Str("model", target.UpstreamModel).
			Err(err).
			Msg("Verification reminder did not produce a build or test call")
		return nil
	}
	return resp
}

type claimCountKey struct{ model, verdict string }

// UnverifiedClaimCount is one row of the detection counter.
type UnverifiedClaimCount struct {
	Model   string
	Verdict string
	Count   int
}

// RecordUnverifiedClaim counts one detected false success claim for model.
func (r *Router) RecordUnverifiedClaim(model, verdict string) {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	r.claimCounts[claimCountKey{model, verdict}]++
}

// UnverifiedClaimCounts returns the detection counter, sorted by model and then verdict.
func (r *Router) UnverifiedClaimCounts() []UnverifiedClaimCount {
	r.claimMu.Lock()
	out := make([]UnverifiedClaimCount, 0, len(r.claimCounts))
	for k, n := range r.claimCounts {
		out = append(out, UnverifiedClaimCount{Model: k.model, Verdict: k.verdict, Count: n})
	}
	r.claimMu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Verdict < out[j].Verdict
	})
	return out
}
