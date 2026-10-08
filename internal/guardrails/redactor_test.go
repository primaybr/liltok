package guardrails

import (
	"strings"
	"testing"
)

func TestScanSecrets(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantTypes []string
	}{
		{
			name:      "clean text",
			input:     "This is a standard prompt explaining binary trees.",
			wantTypes: nil,
		},
		{
			name:      "openai key",
			input:     "Use this key sk-proj-1234567890abcdef1234567890 in config",
			wantTypes: []string{"openai_key"},
		},
		{
			name:      "anthropic key",
			input:     "export ANTHROPIC_API_KEY=sk-ant-api03-abcdef1234567890abcdef1234567890",
			wantTypes: []string{"anthropic_key"},
		},
		{
			name:      "github token",
			input:     "git clone https://ghp_123456789012345678901234567890123456@github.com/repo",
			wantTypes: []string{"github_pat"},
		},
		{
			name:      "aws access key",
			input:     "AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE",
			wantTypes: []string{"aws_access_key"},
		},
		{
			name:      "private key",
			input:     "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA0...",
			wantTypes: []string{"private_key"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			findings := ScanSecrets(tc.input)
			if len(tc.wantTypes) == 0 {
				if len(findings) > 0 {
					t.Fatalf("expected no findings, got %+v", findings)
				}
				if HasSecrets(tc.input) {
					t.Fatalf("expected HasSecrets=false for %q", tc.input)
				}
				return
			}
			if !HasSecrets(tc.input) {
				t.Fatalf("expected HasSecrets=true for %q", tc.input)
			}
			if len(findings) != len(tc.wantTypes) {
				t.Fatalf("expected %d findings, got %d", len(tc.wantTypes), len(findings))
			}
			for i, wt := range tc.wantTypes {
				if findings[i].Type != wt {
					t.Errorf("finding[%d] type = %s, want %s", i, findings[i].Type, wt)
				}
			}
		})
	}
}

func TestRedactSecrets(t *testing.T) {
	input := "My token is sk-proj-1234567890abcdef1234567890 and github token is ghp_123456789012345678901234567890123456."
	redacted := RedactSecrets(input)

	if strings.Contains(redacted, "sk-proj-") {
		t.Errorf("redacted string still contains openai key: %s", redacted)
	}
	if strings.Contains(redacted, "ghp_") {
		t.Errorf("redacted string still contains github token: %s", redacted)
	}
	if !strings.Contains(redacted, "[REDACTED_SECRET:openai_key]") {
		t.Errorf("missing openai redaction placeholder: %s", redacted)
	}
	if !strings.Contains(redacted, "[REDACTED_SECRET:github_pat]") {
		t.Errorf("missing github redaction placeholder: %s", redacted)
	}
}

func TestGithubFineGrainedPATVariableLength(t *testing.T) {
	tok := "github_pat_" + strings.Repeat("A", 40)
	if !HasSecrets(tok) {
		t.Fatalf("expected %q to be detected", tok)
	}
}
