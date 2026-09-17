package semantic

import (
	"regexp"
	"strings"
)

var (
	// Temporal phrases that invalidate semantic caching because responses are time-sensitive
	temporalRegex = regexp.MustCompile(`(?i)\b(today|yesterday|tomorrow|current\s+(?:time|date|day|hour|minute|timestamp|year)|right\s+now|what\s+time\s+is\s+it)\b`)

	// Explicit randomness/nonce keywords
	nonceRegex = regexp.MustCompile(`(?i)\b(random\s+(?:seed|number|string|uuid|id|key)|generate\s+random|nonce)\b`)
)

// GuardrailDecision describes whether a prompt is eligible for semantic similarity caching.
type GuardrailDecision struct {
	IsEligible bool
	Reason     string
}

// CheckSemanticEligibility evaluates if a user prompt and configuration safely allow semantic similarity caching.
func CheckSemanticEligibility(prompt string) GuardrailDecision {
	trimmed := strings.TrimSpace(prompt)
	if len(trimmed) == 0 {
		return GuardrailDecision{IsEligible: false, Reason: "empty prompt"}
	}

	// Temporal / Time-dependent bypass
	if temporalRegex.MatchString(trimmed) {
		return GuardrailDecision{
			IsEligible: false,
			Reason:     "time-dependent or temporal query detected",
		}
	}

	// Randomness / Nonce bypass
	if nonceRegex.MatchString(trimmed) {
		return GuardrailDecision{
			IsEligible: false,
			Reason:     "explicit randomness or nonce requested",
		}
	}

	return GuardrailDecision{
		IsEligible: true,
		Reason:     "qualified for semantic matching",
	}
}
