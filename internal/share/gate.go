// Package share turns questions from a user's own cache into candidates for the shared answer pack.
// Everything here is a pure function of its input: no database, no network.
package share

import (
	"math"
	"net"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/primaybr/liltok/internal/cache/semantic"
)

// GateVersion identifies the rule set. Bump it whenever a rule changes: candidates gated under an
// older version are gated again, and the maintainer rejects submissions made with one.
const GateVersion = 1

// Rule IDs, in the order Check applies them.
const (
	RuleSize     = "size"
	RuleSecret   = "secret"
	RulePII      = "pii"
	RulePath     = "path"
	RuleURL      = "url"
	RuleContext  = "context"
	RuleDenyTerm = "deny_term"
	RuleCode     = "code"
	RuleTemporal = "temporal"
	RuleError    = "error"
)

const (
	minQuestionChars   = 20
	maxQuestionChars   = 2000
	maxCodeBlockLines  = 15
	maxCodeShare       = 0.5
	minEntropyTokenLen = 24
	minTokenEntropy    = 4.0
	minPhoneDigits     = 9
	minDenyTermLen     = 3
)

var (
	secretPattern  = regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{8,}|\bsk-[A-Za-z0-9_\-]{16,}|\bgsk_[A-Za-z0-9]{16,}|\bnvapi-[A-Za-z0-9_\-]{16,}|\bgh[pousr]_[A-Za-z0-9]{20,}|\bgithub_pat_[A-Za-z0-9_]{20,}|\bAIza[0-9A-Za-z_\-]{30,}|\bAKIA[0-9A-Z]{16}\b|\bxox[abp]-[A-Za-z0-9\-]{10,}|\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}|-----BEGIN [A-Z ]*PRIVATE KEY-----|(?i:\bbearer\s+[A-Za-z0-9._~+/\-]{16,})`)
	emailPattern   = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	phonePattern   = regexp.MustCompile(`\+?\(?\d[\d\s().\-]{7,}\d`)
	ipv4Pattern    = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)
	drivePath      = regexp.MustCompile(`\b[A-Za-z]:[\\/]`)
	homePath       = regexp.MustCompile(`(?i)(?:^|[^\w.])(?:/home/|/users/|~/|~\\)`)
	uncPath        = regexp.MustCompile(`(?:^|[^\\])\\\\[A-Za-z0-9._$\-]+\\`)
	urlPattern     = regexp.MustCompile(`(?i)\b(?:https?|ftp)://[^\s"'<>()\[\]{}]+`)
	contextPattern = regexp.MustCompile(`(?i)userEmail|claudeMd|gitStatus|today's date is|working directory|<system-reminder`)
)

// GateConfig holds the per-machine parts of the rules.
type GateConfig struct {
	DenyTerms    []string // names that identify the contributor: projects, employers, people, hosts
	URLAllowlist []string // hosts a question may link to (subdomains included)
}

// Verdict is the gate's decision; Rule is empty when the question passed.
type Verdict struct {
	Rule string
}

// Passed reports whether no rule rejected the question.
func (v Verdict) Passed() bool { return v.Rule == "" }

// Gate applies the rules; build one with NewGate and reuse it.
type Gate struct {
	allow []string
	deny  []*regexp.Regexp
}

// NewGate compiles cfg. Deny terms shorter than three characters are ignored, and each term matches
// only as a whole word (letters, digits and underscores do not continue it), case-insensitively.
func NewGate(cfg GateConfig) *Gate {
	g := &Gate{}
	for _, a := range cfg.URLAllowlist {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			g.allow = append(g.allow, a)
		}
	}
	seen := map[string]bool{}
	for _, t := range cfg.DenyTerms {
		t = strings.TrimSpace(t)
		key := strings.ToLower(t)
		if utf8.RuneCountInString(t) < minDenyTermLen || seen[key] {
			continue
		}
		seen[key] = true
		g.deny = append(g.deny, regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}_])`+regexp.QuoteMeta(t)+`(?:$|[^\p{L}\p{N}_])`))
	}
	return g
}

// Check returns the first rule the question breaks, in the documented order. Any internal failure
// rejects the question with RuleError.
func (g *Gate) Check(question string) (v Verdict) {
	defer func() {
		if recover() != nil {
			v = Verdict{Rule: RuleError}
		}
	}()
	if !utf8.ValidString(question) {
		return Verdict{Rule: RuleError}
	}
	q := strings.TrimSpace(question)
	if n := utf8.RuneCountInString(q); n < minQuestionChars || n > maxQuestionChars {
		return Verdict{Rule: RuleSize}
	}
	if hasSecret(q) {
		return Verdict{Rule: RuleSecret}
	}
	if hasPII(q) {
		return Verdict{Rule: RulePII}
	}
	if hasPath(q) {
		return Verdict{Rule: RulePath}
	}
	if g.hasForeignURL(q) {
		return Verdict{Rule: RuleURL}
	}
	if contextPattern.MatchString(q) {
		return Verdict{Rule: RuleContext}
	}
	for _, re := range g.deny {
		if re.MatchString(q) {
			return Verdict{Rule: RuleDenyTerm}
		}
	}
	if hasCodeDump(q) {
		return Verdict{Rule: RuleCode}
	}
	if !semantic.CheckSemanticEligibility(q).IsEligible {
		return Verdict{Rule: RuleTemporal}
	}
	return Verdict{}
}

// tokens splits on whitespace and on quoting and bracketing punctuation.
func tokens(q string) []string {
	return strings.FieldsFunc(q, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("\"'`()[]{}<>,;", r)
	})
}

func hasSecret(q string) bool {
	if secretPattern.MatchString(q) {
		return true
	}
	for _, tok := range tokens(q) {
		// URLs are judged by the url rule; a random-looking key mixes letters and digits.
		if len(tok) < minEntropyTokenLen || strings.Contains(tok, "://") || !hasLetterAndDigit(tok) {
			continue
		}
		if shannonEntropy(tok) > minTokenEntropy {
			return true
		}
	}
	return false
}

func hasLetterAndDigit(s string) bool {
	var letter, digit bool
	for _, r := range s {
		letter = letter || unicode.IsLetter(r)
		digit = digit || unicode.IsDigit(r)
	}
	return letter && digit
}

func shannonEntropy(s string) float64 {
	counts := map[rune]int{}
	n := 0
	for _, r := range s {
		counts[r]++
		n++
	}
	var h float64
	for _, c := range counts {
		p := float64(c) / float64(n)
		h -= p * math.Log2(p)
	}
	return h
}

func hasPII(q string) bool {
	if emailPattern.MatchString(q) {
		return true
	}
	for _, m := range phonePattern.FindAllString(q, -1) {
		digits := 0
		for _, r := range m {
			if r >= '0' && r <= '9' {
				digits++
			}
		}
		if digits >= minPhoneDigits {
			return true
		}
	}
	for _, m := range ipv4Pattern.FindAllString(q, -1) {
		if ip := net.ParseIP(m); ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
			return true
		}
	}
	for _, tok := range tokens(q) {
		if strings.Count(tok, ":") < 2 {
			continue
		}
		t := strings.TrimRight(strings.Trim(tok, "[]"), ".?!")
		if i := strings.LastIndex(t, "%"); i >= 0 {
			t = t[:i]
		}
		if ip := net.ParseIP(t); ip != nil && ip.To4() == nil && !ip.IsLoopback() && !ip.IsUnspecified() {
			return true
		}
	}
	return false
}

func hasPath(q string) bool {
	if drivePath.MatchString(q) || homePath.MatchString(q) || uncPath.MatchString(q) {
		return true
	}
	for _, tok := range tokens(q) {
		if strings.Contains(tok, "://") {
			continue
		}
		tok = strings.TrimRight(tok, ".:!?")
		for _, sep := range []string{"/", `\`} {
			segments := 0
			for _, s := range strings.Split(tok, sep) {
				if s != "" {
					segments++
				}
			}
			if segments >= 3 {
				return true
			}
		}
	}
	return false
}

func (g *Gate) hasForeignURL(q string) bool {
	for _, raw := range urlPattern.FindAllString(q, -1) {
		u, err := url.Parse(strings.TrimRight(raw, ".,:;!?"))
		if err != nil || !g.hostAllowed(strings.ToLower(u.Hostname())) {
			return true
		}
	}
	return false
}

func (g *Gate) hostAllowed(host string) bool {
	if host == "" {
		return false
	}
	for _, a := range g.allow {
		if host == a || strings.HasSuffix(host, "."+a) {
			return true
		}
	}
	return false
}

// hasCodeDump reports a fenced block longer than maxCodeBlockLines, or fenced code making up more
// than maxCodeShare of the question. An unterminated fence counts to the end of the question.
func hasCodeDump(q string) bool {
	inFence := false
	blockLines, codeChars := 0, 0
	for _, line := range strings.Split(q, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			if inFence && blockLines > maxCodeBlockLines {
				return true
			}
			inFence = !inFence
			blockLines = 0
			continue
		}
		if inFence {
			blockLines++
			codeChars += len(line) + 1
		}
	}
	if inFence && blockLines > maxCodeBlockLines {
		return true
	}
	return float64(codeChars) > maxCodeShare*float64(len(q))
}
