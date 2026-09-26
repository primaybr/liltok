package share_test

import (
	"strings"
	"testing"

	"github.com/primaybr/liltok/internal/share"
)

var testAllowlist = []string{"go.dev", "pkg.go.dev", "docs.python.org"}

func fence(lines int) string {
	return "```go\n" + strings.Repeat("x := 1\n", lines) + "```\n"
}

func TestGate(t *testing.T) {
	g := share.NewGate(share.GateConfig{
		DenyTerms:    []string{"acme-billing", "c++", "dev", "ab"},
		URLAllowlist: testAllowlist,
	})
	cases := []struct {
		name, q, want string
	}{
		{"plain question passes", "How do I reverse a slice in Go without allocating a new one?", ""},
		{"too short", "How to sort?", share.RuleSize},
		{"too long", strings.Repeat("word ", 450), share.RuleSize},
		{"openai-style key", "Why does my client fail with key sk-proj-abcdefghijklmnop1234 set?", share.RuleSecret},
		{"aws key id", "Is AKIAIOSFODNN7EXAMPLE a valid access key id format?", share.RuleSecret},
		{"jwt", "Why does eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.abcdefghijk fail to verify?", share.RuleSecret},
		{"pem", "Why does -----BEGIN RSA PRIVATE KEY----- fail to parse in Go?", share.RuleSecret},
		{"high entropy token", "Why is x9Qz4Lm2Vb8Nc1Xr7Tk3Wp6Yh0 rejected by the API gateway?", share.RuleSecret},
		{"email", "Why does dev@corp.example.com not receive the reset email?", share.RulePII},
		{"phone", "Should I store +62 812 3456 7890 style numbers as strings in Postgres?", share.RulePII},
		{"private ipv4", "Why can't my laptop reach 10.1.2.3 from inside Docker?", share.RulePII},
		{"ipv6", "Why can't my container reach fd00::1:2 over the bridge network?", share.RulePII},
		{"loopback passes", "Why does 127.0.0.1 refuse connections from inside a Docker container?", ""},
		{"drive path", `Why does go build fail in C:\Users\alice\proj on Windows?`, share.RulePath},
		{"home path", "Why can't the service read /home/alice/app/config.yaml at boot?", share.RulePath},
		{"tilde path", "Should I keep ~/work/app under version control or not?", share.RulePath},
		{"unc path", `How do I mount \\fileserver\share\x from WSL reliably?`, share.RulePath},
		{"deep relative path", "Why does src/internal/billing/tax.go fail to compile in CI?", share.RulePath},
		{"two-segment path passes", "What is the difference between io/fs and the os package in Go?", ""},
		{"foreign url", "Is the approach at https://wiki.corp.example/go-style any good?", share.RuleURL},
		{"allowlisted url passes", "The page https://docs.python.org/3/library/asyncio-task.html mentions TaskGroup; how does it cancel?", ""},
		{"mixed urls", "Compare https://go.dev/doc/effective_go with https://wiki.corp.example/go-style please", share.RuleURL},
		{"context markers", "Given userEmail and gitStatus fields, how should a CLI print them?", share.RuleContext},
		{"deny term", "How does acme-billing retry failed invoices after a timeout?", share.RuleDenyTerm},
		{"deny term with metacharacters", "Is c++ really faster than Rust for writing parsers?", share.RuleDenyTerm},
		{"deny term inside a word passes", "What makes a good developer onboarding document?", ""},
		{"short deny term ignored", "How do ab tests work at scale in practice?", ""},
		{"long code block", "Why does this loop leak memory?\n" + fence(16), share.RuleCode},
		{"code majority", "Explain:\n```\n" + strings.Repeat("fmt.Println(strings.Repeat(\"abc\", 20))\n", 3) + "```", share.RuleCode},
		{"short code block passes", "Why does the compiler reject the following when the map value is a struct and I assign a field directly?\n" + fence(3), ""},
		{"temporal", "What is the current time in Jakarta right now, roughly?", share.RuleTemporal},
		{"invalid utf-8", "\xff\xfe How do I reverse a slice in Go quickly?", share.RuleError},
	}
	for _, tc := range cases {
		if got := g.Check(tc.q).Rule; got != tc.want {
			t.Errorf("%s: rule = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestVerdictPassed(t *testing.T) {
	if !(share.Verdict{}).Passed() || (share.Verdict{Rule: share.RulePII}).Passed() {
		t.Error("Passed must be true only for an empty rule")
	}
}
