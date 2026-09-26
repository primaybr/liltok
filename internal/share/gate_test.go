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

// openFence returns a fenced code block that is never closed, so the fence runs to the end of the
// question.
func openFence(lines int) string {
	return "```go\n" + strings.Repeat("x := 1\n", lines)
}

func TestGate(t *testing.T) {
	g := share.NewGate(share.GateConfig{
		DenyTerms:    []string{"acme-billing", "c++", "dev", "ab", "acmehq"},
		URLAllowlist: testAllowlist,
	})
	cases := []struct {
		name, q, want string
	}{
		{"plain question passes", "How do I reverse a slice in Go without allocating a new one?", ""},
		{"too short", "How to sort?", share.RuleSize},
		{"too long", strings.Repeat("word ", 450), share.RuleSize},
		{"exactly 19 runes is too short", "Why use goroutines?", share.RuleSize},
		{"exactly 20 runes passes", "Why do Go maps leak?", ""},
		{"openai-style key", "Why does my client fail with key sk-proj-abcdefghijklmnop1234 set?", share.RuleSecret},
		{"aws key id", "Is AKIAIOSFODNN7EXAMPLE a valid access key id format?", share.RuleSecret},
		{"jwt", "Why does eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.abcdefghijk fail to verify?", share.RuleSecret},
		{"pem", "Why does -----BEGIN RSA PRIVATE KEY----- fail to parse in Go?", share.RuleSecret},
		{"high entropy token", "Why is x9Qz4Lm2Vb8Nc1Xr7Tk3Wp6Yh0 rejected by the API gateway?", share.RuleSecret},
		{"hex secret", "Is 3f2504e04f8911d39a0c0305e82c3301 a valid 32-character hex identifier?", share.RuleSecret},
		{"plaintext credential", "Why does login fail with password=Summer2024! set in the config?", share.RuleSecret},
		{"password in connection string", "Why does postgres://admin:hunter2@localhost:5432/app refuse my login?", share.RuleSecret},
		{"email", "Why does dev@corp.example.com not receive the reset email?", share.RulePII},
		{"phone", "Should I store +62 812 3456 7890 style numbers as strings in Postgres?", share.RulePII},
		{"phone with nbsp", "Should I store +62\u00A0812\u00A03456\u00A07890 style numbers as strings in Postgres?", share.RulePII},
		{"email with zero-width space", "Why does alice\u200B@corp.example.com not receive the reset email?", share.RulePII},
		{"private ipv4", "Why can't my laptop reach 10.1.2.3 from inside Docker?", share.RulePII},
		{"ipv6", "Why can't my container reach fd00::1:2 over the bridge network?", share.RulePII},
		{"ipv6 cidr", "How do I route fd12:3456:789a::/48 over the tunnel?", share.RulePII},
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
		// These two use a docs.python.org subdomain/host rather than go.dev: go.dev ends in
		// "dev", and under the deny-term boundary rule a dot or slash counts as a boundary, so
		// mentioning go.dev would also (correctly, per the deny_term rule) match the short deny
		// term "dev" used elsewhere in this fixture and reach that rule before reaching "".
		{"subdomain of allowlisted host passes", "The subdomain page at https://asyncio.docs.python.org/task.html is a good start; what should I read after it?", ""},
		{"uppercase host passes", "Please review HTTPS://DOCS.PYTHON.ORG/3/library/asyncio-task.html for the TaskGroup behavior.", ""},
		{"private host via other scheme", "Why does redis://cache.corp.example:6379/0 keep dropping connections under load?", share.RuleURL},
		{"private host via websocket scheme", "Why does wss://api.corp.example/stream disconnect every few minutes exactly?", share.RuleURL},
		{"bare host with path", "I read the wiki.corp.example/go-style page but still don't understand the rule.", share.RuleURL},
		{"context markers", "Given userEmail and gitStatus fields, how should a CLI print them?", share.RuleContext},
		{"deny term", "How does acme-billing retry failed invoices after a timeout?", share.RuleDenyTerm},
		{"deny term with metacharacters", "Is c++ really faster than Rust for writing parsers?", share.RuleDenyTerm},
		{"deny term inside a word passes", "What makes a good developer onboarding document?", ""},
		{"short deny term ignored", "How do ab tests work at scale in practice?", ""},
		{"long deny term matches an identifier built on it (camel case)", "Why does AcmeHQClient panic when the internal queue backs up unexpectedly?", share.RuleDenyTerm},
		{"long deny term matches an identifier built on it (snake case)", "How does the acmehq_billing job retry after a timeout without duplicating rows?", share.RuleDenyTerm},
		{"short deny term treats underscore as a boundary", "Why does dev_tools fail to install the linter plugin on a clean machine?", share.RuleDenyTerm},
		{"long code block", "Why does this loop leak memory?\n" + fence(16), share.RuleCode},
		{"code majority", "Explain:\n```\n" + strings.Repeat("fmt.Println(strings.Repeat(\"abc\", 20))\n", 3) + "```", share.RuleCode},
		{"short code block passes", "Why does the compiler reject the following when the map value is a struct and I assign a field directly?\n" + fence(3), ""},
		{"exactly 15 fenced lines passes", "Why does the compiler reject the following when the map value is a struct and I assign a field directly?\n" + fence(15), ""},
		{"unterminated fence of 16 lines rejects", "Why does this loop leak memory?\n" + openFence(16), share.RuleCode},
		{"temporal", "What is the current time in Jakarta right now, roughly?", share.RuleTemporal},
		{"invalid utf-8", "\xff\xfe How do I reverse a slice in Go quickly?", share.RuleError},
		// Round-2 findings: the credential pattern's leading \b never fires right after an
		// underscore (an env-var name like DB_PASSWORD never gets a word-boundary match there),
		// and it required the separator to follow the keyword immediately, so a quoted JSON key
		// like "password": ... also passed.
		{"env-style credential (underscore prefix)", "I keep seeing DB_PASSWORD=hunter2 in the container logs; is that normal?", share.RuleSecret},
		{"env-style credential secret name", "The staging service prints CLIENT_SECRET=abcd1234 to stdout on every boot.", share.RuleSecret},
		{"exported env-style token", "The deploy script runs export GITHUB_TOKEN=abc123 before calling the API.", share.RuleSecret},
		{"snake-case keyword with colon", "The onboarding doc says to set my_api_key: abcd1234efgh in the shell profile.", share.RuleSecret},
		{"quoted json credential", "The debug dump includes {\"password\": \"hunter2\"} right in the response body.", share.RuleSecret},
		// Round-2 findings: the bare-host pattern required "/" immediately after the final
		// label, so a host with a port, a protocol-relative "//host/path" mention, or a
		// markdown-decorated "**host/path**" mention all passed.
		{"bare host with port", "Why does db.corp.io:5432/app refuse connections from the staging pod?", share.RuleURL},
		{"protocol-relative host", "The asset loads from //cdn.corp.example/lib.js without specifying a scheme.", share.RuleURL},
		{"markdown-decorated host", "The internal wiki says **wiki.corp.example/go-style** is the canonical guide.", share.RuleURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := g.Check(tc.q).Rule; got != tc.want {
				t.Errorf("rule = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGateNoDenyTerms covers allowlist-only cases with a gate that has no deny terms, since
// go.dev and pkg.go.dev both end in "dev": with the fixture's "dev" deny term in play (as in
// TestGate's gate), mentioning either would also, correctly, match that deny term and reach
// deny_term before reaching a passing "" verdict. These cases isolate the bare-host allowlist
// logic (round-2 finding 2) from that unrelated collision.
func TestGateNoDenyTerms(t *testing.T) {
	g := share.NewGate(share.GateConfig{URLAllowlist: testAllowlist})
	cases := []struct {
		name, q, want string
	}{
		{"pkg.go.dev bare host passes", "The pkg.go.dev/errors package documents how to wrap and unwrap errors nicely.", ""},
		{"two-segment path passes", "What is the difference between io/fs and the os package in Go?", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := g.Check(tc.q).Rule; got != tc.want {
				t.Errorf("rule = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestVerdictPassed(t *testing.T) {
	if !(share.Verdict{}).Passed() || (share.Verdict{Rule: share.RulePII}).Passed() {
		t.Error("Passed must be true only for an empty rule")
	}
}
