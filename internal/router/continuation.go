package router

import (
	"context"
	"errors"
	"time"

	"github.com/primaybr/liltok/internal/provider"
	"github.com/primaybr/liltok/internal/telemetry"
)

// maxContinuationAttempts bounds the targets asked to finish a stalled, already-streamed turn.
const maxContinuationAttempts = 3

// continuationPrompt follows a stalled turn in the continuation request. It is sent as a user
// message with the same "[System Reminder]" prefix the OpenAI adapter gives injected reminders.
const continuationPrompt = "[System Reminder] Your previous message announced its next step but did not call a tool, " +
	"so nothing ran. Make that tool call now. Reply with the tool call only; do not repeat the message."

// continueTurn finishes a live-streamed turn whose text already reached the client but must not be
// the end of the run: a stalled turn that announced a tool action without calling it, or a final
// reply that claimed a passing build or test run nobody verified. Instead of failing the turn
// (which ends the agent run) it asks for the missing call: the conversation plus the streamed text
// as an assistant turn plus prompt, sent first to the target that produced the reply, then to
// rest, the chain's later targets. The returned reply keeps the streamed text and carries the new
// tool calls; the continuation's own text is dropped so the client does not see the step twice. It
// returns nil when no target produced a valid tool call.
func (r *Router) continueTurn(ctx context.Context, req *provider.UnifiedChatRequest, stalled *provider.UnifiedChatResponse, current TargetSpec, rest []TargetSpec, approxTokens int, prompt string) (*provider.UnifiedChatResponse, string) {
	contReq := *req
	contReq.RawPayload = nil // providers pass a raw OpenAI payload through; build from Messages instead
	contReq.Stream = false
	contReq.Messages = append(append([]provider.UnifiedChatMessage(nil), req.Messages...),
		provider.UnifiedChatMessage{Role: "assistant", Content: visibleReplyText(stalled)},
		provider.UnifiedChatMessage{Role: "user", Content: prompt},
	)

	order := append([]TargetSpec{current}, rest...)

	attempts := 0
	for _, target := range order {
		if attempts >= maxContinuationAttempts || ctx.Err() != nil {
			break
		}
		target, p, cb, ok := r.dispatchableTarget(target, approxTokens)
		if !ok {
			continue
		}
		attempts++

		targetReq := contReq
		targetReq.Model = target.UpstreamModel
		attemptCtx, cancel := ctx, context.CancelFunc(func() {})
		if r.attemptTimeout > 0 && p.Tier() != provider.TierPremium {
			attemptCtx, cancel = context.WithTimeout(ctx, r.attemptTimeout)
		}
		start := time.Now()
		resp, err := p.SendChat(attemptCtx, &targetReq)
		cancel()
		raw := snapshotResponse(resp)
		if err == nil {
			err = checkReply(&contReq, target, resp)
		}
		if err == nil && len(resp.ToolCalls) == 0 {
			err = errors.New("continuation made no tool call")
		}
		observeAttempt(ctx, AttemptResult{Provider: target.ProviderName, Model: target.UpstreamModel, Response: raw, Err: err, Latency: time.Since(start)})
		if err != nil {
			if isCircuitBreakerError(err) && !errors.Is(err, errStalledTurn) {
				cb.RecordFailure()
			}
			telemetry.Log.Warn().
				Str("provider", target.ProviderName).
				Str("model", target.UpstreamModel).
				Err(err).
				Msg("Continuation of a live turn failed")
			continue
		}
		cb.RecordSuccess()

		merged := *stalled
		merged.ToolCalls = resp.ToolCalls
		merged.FinishReason = "tool_calls"
		merged.Usage.PromptTokens += resp.Usage.PromptTokens
		merged.Usage.CompletionTokens += resp.Usage.CompletionTokens
		merged.Usage.TotalTokens += resp.Usage.TotalTokens
		telemetry.Log.Info().
			Str("stalled_model", current.UpstreamModel).
			Str("provider", target.ProviderName).
			Str("model", target.UpstreamModel).
			Str("tool", resp.ToolCalls[0].Function.Name).
			Msg("Recovered a live turn with a continuation tool call")
		return &merged, target.ProviderName
	}
	return nil, ""
}

// dispatchableTarget applies the per-target skips of DispatchChat's chain (context window, model
// alias and listing, provider-reported availability, excluded models, open breaker) and returns
// the resolved target with its provider and breaker.
func (r *Router) dispatchableTarget(target TargetSpec, approxTokens int) (TargetSpec, provider.ProviderClient, *CircuitBreaker, bool) {
	if approxTokens > r.GetModelContextWindow(target.ProviderName, target.UpstreamModel) {
		return target, nil, nil, false
	}
	if catalogedProviders[target.ProviderName] {
		if actual, ok := ResolveModelAlias(target.ProviderName, target.UpstreamModel); ok {
			target.UpstreamModel = actual
		}
		if !r.IsActiveModel(target.ProviderName, target.UpstreamModel) {
			return target, nil, nil, false
		}
	}
	if !r.catalog.Usable(target.ProviderName, target.UpstreamModel) || r.isExcludedModel(target.UpstreamModel) {
		return target, nil, nil, false
	}
	p, exists := r.providers[target.ProviderName]
	if !exists {
		return target, nil, nil, false
	}
	cb := r.getTargetBreaker(target.ProviderName, target.UpstreamModel)
	if !cb.Allow() {
		return target, nil, nil, false
	}
	return target, p, cb, true
}
