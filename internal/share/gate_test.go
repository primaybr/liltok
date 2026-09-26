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
		// Task 7-fix, item 6: a slash-joined run of short English words, a stdlib import path, and
		// a golang.org/x/... module path all read as prose or code, not a filesystem or URL path.
		// The golang.org/x case also exercises the matching exemption added to hasForeignURL's
		// bare-host check, since golang.org/x/sync/errgroup would otherwise match bareHostPattern
		// and reject as a foreign host before the path rule is ever reached.
		{"path exemption: short words joined by slashes", "Should the article be a/the/an before an acronym like URL?", ""},
		{"path exemption: stdlib import path", "How do I read a heap profile from net/http/pprof in production?", ""},
		{"path exemption: golang.org/x import path", "How does golang.org/x/sync/errgroup cancel siblings?", ""},
		{"non-exempt path with capitalized segments still rejects", "Why does App/Modules/Billing/Controllers fail to autoload?", share.RulePath},
		{"non-exempt path with a file extension still rejects", "How do I test src/app/main.go?", share.RulePath},
		// Fix round 1, item 1: exemption (a) checked only the first segment, so a real path nested
		// under a stdlib-named directory passed as if it were an import path. It now also requires
		// the whole token to be nothing but lowercase-alphanumeric segments.
		{"stdlib-prefixed non-import path still rejects (underscore and extension)", "Why does database/migrations/create_users.php fail during the nightly deploy?", share.RulePath},
		{"stdlib-prefixed non-import path still rejects (dotted segment)", "Why does go/pkg/mod/github.com/secretco/billing/tax.go fail to build in CI?", share.RulePath},
		{"stdlib-prefixed non-import path still rejects (deep dotted segment)", "Why does go/src/github.com/secretco/billing fail go vet in CI?", share.RulePath},
		{"stdlib-prefixed non-import path still rejects (mixed case segment)", "Why can't the loader find path/to/Secret/Config.yaml at startup?", share.RulePath},
		{"stdlib-prefixed non-import path still rejects (parent-dir segments)", "Why does the setup script read os/exec/../../etc/shadow by mistake?", share.RulePath},
		{"stdlib import path with all-lowercase segments still passes", "How does database/sql/driver differ from database/sql itself?", ""},
		// Fix round 1, item 4: exemption (b) used to allow up to 4 segments and any lowercase word,
		// so a real relative path under a common directory name passed as if it were prose.
		{"ordinary-words exemption no longer covers a common directory (4 segments)", "Why does home/alice/secret/data fail to sync after the migration?", share.RulePath},
		{"ordinary-words exemption no longer covers a common directory (3 segments)", "Why does opt/billing/tax fail to reconcile nightly?", share.RulePath},
		// Accepted residual (documented, not fixed): a plausible bare project path of three ordinary
		// words with no common-directory segment still passes exemption (b); catching a real project
		// name this way depends on it also being configured as a deny term.
		{"three ordinary words still pass (residual gap; use a deny term to close it)", "How does acme/billing/tax compare with the old spreadsheet workflow?", ""},
		// Task 7-fix, item 7: an ISO-8601 timestamp is time-anchored the same way "today" is.
		{"iso timestamp", "Why did the nightly build at 2031-02-03T10:11 fail to upload artifacts?", share.RuleTemporal},
		// Task 7-fix, item 1: echo/liveness probes ask for a fixed literal reply, not an explanation.
		{"probe: reply with exactly", "Reply with exactly: 'Widget service is online.'", share.RuleProbe},
		{"probe: say in one word", "Say ping in exactly one word.", share.RuleProbe},
		{"probe: respond with status ok", "The healthcheck script should respond with status ok when the port answers.", share.RuleProbe},
		{"probe: calculate just the number", "What is 312 + 45? Answer with just the number.", share.RuleProbe},
		{"probe lookalike passes", `What does "reply-to" mean in an SMTP header and when is it only advisory?`, ""},
		// Task 7-fix, item 2: a question that asserts its own premise up front, rather than asking
		// about it, is how the round-1 dry run's leaked candidates were phrased.
		{"assertion: confirm that", "Confirm that storing frob_bytes in widget_logs gives accurate savings for Acme sessions.", share.RuleAssertion},
		{"assertion lookalike passes", "How do I verify that a TLS certificate chain is complete with openssl?", ""},
		// Task 7-fix2, item 1: a line that is only a "paste your X below" label, with pasted
		// material somewhere after it, is how the round-2 dry run's text-compression leaks were
		// shaped.
		{"payload: label line then pasted material", "Compress this log. Rules: keep all facts.\n\nText:\n\n## 09:12 | main\nFixed WidgetSync retry in widget_jobs.", share.RulePayload},
		{"payload: input label then pasted material", "Summarise the following.\nInput:\nAcme ops notes for week 12", share.RulePayload},
		{"payload lookalike (label word inline, not on its own line) passes", `In a multipart/form-data body, what does the Content-Type: text/plain line of a part mean?`, ""},
		// Task 7-fix2, item 2: a markdown heading that opens with a clock-style timestamp is the
		// heading a pasted log excerpt keeps once it is copied into a question.
		{"payload: timestamped log heading", "## 14:05 | develop\nDeployed AcmeBilling v2.3 to the cluster. What should I check next?", share.RulePayload},
		{"payload lookalike (no heading marker) passes", "Why does cron treat 0 9 * * 1-5 as 09:00 on weekdays?", ""},
		// Task 7-fix2, item 3: a dashed UUID names one specific private run, so it is judged secret.
		{"dashed uuid", "Why did run 3f2a9c1e-7b4d-4e2a-9c1f-0a1b2c3d4e5f stall at 14%?", share.RuleSecret},
		{"uuid discussion without a literal uuid passes", "What is the difference between UUID v4 and UUID v7 layouts?", ""},
		// Task 7-fix2, item 4: three more probe shapes - a role-play opener, a short one-word-answer
		// request, and a trailing token/nonce value.
		{"probe: role-play opener (in)", "You are in plan mode. Write a 3-step plan for adding a widget cache.", share.RuleProbe},
		{"probe: role-play opener (summarizing)", "You are summarizing a session for a daily log. Rules: keep facts.", share.RuleProbe},
		{"probe lookalike (are you, not you are) passes", "Are you required to close a Go http.Response body?", ""},
		{"probe: one-word answer on a short prompt", "Calculate 3+5 and answer in one word.", share.RuleProbe},
		{"probe: one-word answer, digit form", "What is the capital of Peru? Answer in 1 word.", share.RuleProbe},
		{"probe lookalike (one-word phrase, but not a request for one) passes", "Why does the word 'answer' in one locale sort differently in ICU collation?", ""},
		{"probe: trailing token nonce", "Explain a write-ahead log in 150 words. Token 9c1e.", share.RuleProbe},
		{"probe lookalike (token with no trailing value) passes", "How do I refresh an OAuth access token?", ""},
		// Task 7-fix2, item 5: path exemption (a) strips one trailing ".Identifier" whose first
		// letter is uppercase before its whole-token check, so an import path mentioned together
		// with one of its own exported names still reads as an import path.
		{"path exemption: stdlib import path with a trailing exported identifier", "How does net/http/httputil.ReverseProxy rewrite the Host header?", ""},
		{"non-exempt stdlib-prefixed path with a lowercase extension still rejects", "Why does net/http/secret.go fail to build?", share.RulePath},
		{"non-exempt path with a trailing capitalized field (first segment not stdlib) still rejects", "How do I test app/billing/Tax.Rate?", share.RulePath},
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

// TestGateDenyTermBoundary covers task 7-fix item 3: a deny term of five or more characters now
// matches a boundary-safe substring occurrence instead of matching anywhere unconditionally, so it
// catches an identifier built from the term (PascalCase, snake_case, or a camelCase run) without
// also matching an unrelated word that merely happens to start the same way.
func TestGateDenyTermBoundary(t *testing.T) {
	g := share.NewGate(share.GateConfig{DenyTerms: []string{"Mater"}, URLAllowlist: testAllowlist})
	cases := []struct {
		name, q, want string
	}{
		{"unrelated word that starts the same way passes", "How do I pick a Material UI theme in React?", ""},
		{"PascalCase identifier matches", "Why does MaterClient retry twice?", share.RuleDenyTerm},
		{"snake_case identifier matches", "How should the mater_billing table be designed?", share.RuleDenyTerm},
		{"camelCase boundary matches", "Why does myMaterClient leak goroutines?", share.RuleDenyTerm},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := g.Check(tc.q).Rule; got != tc.want {
				t.Errorf("rule = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGateDenyTermBoundaryFixRound1 covers fix round-1 items 2 and 3: probing the boundary rule
// from TestGateDenyTermBoundary found it still missed an acronym, all-caps, digit, or CJK rune
// immediately before the term, and a plain plural or past-tense suffix immediately after it. Both
// sides were narrowed; "mywidgetgate" (a glued, unbroken lowercase run on the left) is the one
// documented residual the ruling explicitly kept.
func TestGateDenyTermBoundaryFixRound1(t *testing.T) {
	g := share.NewGate(share.GateConfig{DenyTerms: []string{"widgetgate"}, URLAllowlist: testAllowlist})
	cases := []struct {
		name, q, want string
	}{
		{"digit immediately before the term now rejects", "Why does v2widgetgate throttle unexpectedly?", share.RuleDenyTerm},
		{"acronym immediately before the term now rejects", "Why does APIWidgetgate crash under load?", share.RuleDenyTerm},
		{"all-caps run around the term now rejects", "Why does MATERIALWIDGETGATEX time out under load?", share.RuleDenyTerm},
		{"CJK rune immediately before the term now rejects", "我们的widgetgate服务为什么会随机重启？", share.RuleDenyTerm},
		{"glued lowercase run is the accepted residual and still passes", "Why does mywidgetgate throttle without warning?", ""},
		{"plural suffix now rejects", "How do I configure widgetgates across the cluster?", share.RuleDenyTerm},
		{"past-tense suffix now rejects", "Why does the healthcheck endpoint report widgetgated after a retry?", share.RuleDenyTerm},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := g.Check(tc.q).Rule; got != tc.want {
				t.Errorf("rule = %q, want %q", got, tc.want)
			}
		})
	}
}
