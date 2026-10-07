package guardrails

import (
	"regexp"
)

var secretPatterns = []struct {
	name    string
	pattern *regexp.Regexp
}{
	{"anthropic_key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}\b`)},
	{"openai_key", regexp.MustCompile(`\bsk-(?:proj-|admin-)?[A-Za-z0-9]{20,}\b`)},
	{"github_pat", regexp.MustCompile(`\bghp_[A-Za-z0-9]{36}\b`)},
	{"github_fine", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{82}\b`)},
	{"aws_access_key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"slack_token", regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9_-]{10,}\b`)},
	{"bearer_token", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/-]{20,}\b`)},
	{"private_key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
}

// SecretFinding describes a detected sensitive secret.
type SecretFinding struct {
	Type  string
	Match string
}

// ScanSecrets returns all detected sensitive tokens in text.
func ScanSecrets(text string) []SecretFinding {
	if text == "" {
		return nil
	}
	var findings []SecretFinding
	for _, p := range secretPatterns {
		matches := p.pattern.FindAllString(text, -1)
		for _, m := range matches {
			findings = append(findings, SecretFinding{
				Type:  p.name,
				Match: m,
			})
		}
	}
	return findings
}

// HasSecrets returns true if any credential or secret is detected.
func HasSecrets(text string) bool {
	if text == "" {
		return false
	}
	for _, p := range secretPatterns {
		if p.pattern.MatchString(text) {
			return true
		}
	}
	return false
}

// RedactSecrets replaces all detected sensitive credentials with [REDACTED_SECRET:type].
func RedactSecrets(text string) string {
	if text == "" {
		return text
	}
	out := text
	for _, p := range secretPatterns {
		out = p.pattern.ReplaceAllString(out, "[REDACTED_SECRET:"+p.name+"]")
	}
	return out
}
