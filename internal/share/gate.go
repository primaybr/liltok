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

// Rule IDs, in the order Check applies them. RuleError is the one exception to that order: an
// invalid-UTF-8 or otherwise unreadable question is rejected with RuleError before any other rule
// runs, even though it is declared last here. Fail-closed means an unreadable question never
// reaches a rule that might wave it through.
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
	minQuestionChars        = 20
	maxQuestionChars        = 2000
	maxCodeBlockLines       = 15
	maxCodeShare            = 0.5
	minEntropyTokenLen      = 24
	minTokenEntropy         = 4.0
	minPhoneDigits          = 9
	minDenyTermLen          = 3
	minSubstringDenyTermLen = 5
)

var (
	secretPattern = regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{8,}|\bsk-[A-Za-z0-9_\-]{16,}|\bgsk_[A-Za-z0-9]{16,}|\bnvapi-[A-Za-z0-9_\-]{16,}|\bgh[pousr]_[A-Za-z0-9]{20,}|\bgithub_pat_[A-Za-z0-9_]{20,}|\bAIza[0-9A-Za-z_\-]{30,}|\bAKIA[0-9A-Z]{16}\b|\bxox[abp]-[A-Za-z0-9\-]{10,}|\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}|-----BEGIN [A-Z ]*PRIVATE KEY-----|(?i:\bbearer\s+[A-Za-z0-9._~+/\-]{16,})`)
	// hexSecretPattern catches a long hex-only run (a hex-encoded key or a UUID used as one):
	// Shannon entropy tops out at exactly 4.0 for a 16-symbol alphabet, so it can never exceed
	// minTokenEntropy and needs its own check.
	hexSecretPattern = regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`)
	// credentialPattern catches a plain "keyword = value" or "keyword: value" credential that has
	// no distinctive shape of its own, such as password=Summer2024!. The leading boundary is
	// "not a letter or digit" rather than \b, so an underscore still counts as a boundary (an
	// env-var name like DB_PASSWORD or GITHUB_TOKEN triggers); an optional closing quote is
	// allowed between the keyword and the separator, so a quoted JSON key like "password": ...
	// also triggers.
	credentialPattern = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])(?:pass(?:word|wd)?|pwd|secret|api[_-]?key|access[_-]?token|auth[_-]?token|token)["']?\s*[:=]\s*\S+`)
	emailPattern      = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	// phonePattern uses [ \t] rather than \s so a match cannot span a newline.
	phonePattern = regexp.MustCompile(`\+?\(?\d[\d \t().\-]{7,}\d`)
	ipv4Pattern  = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)
	drivePath    = regexp.MustCompile(`\b[A-Za-z]:[\\/]`)
	homePath     = regexp.MustCompile(`(?i)(?:^|[^\w.])(?:/home/|/users/|~/|~\\)`)
	uncPath      = regexp.MustCompile(`(?:^|[^\\])\\\\[A-Za-z0-9._$\-]+\\`)
	// urlPattern accepts any scheme, not just http(s)/ftp, so a scheme like postgres:// or redis://
	// is judged instead of silently passing.
	urlPattern = regexp.MustCompile(`(?i)\b[A-Za-z][A-Za-z0-9+.\-]*://[^\s"'<>()\[\]{}]+`)
	// bareHostPattern matches a scheme-less "host.tld/path" mention: one or more dotted labels
	// ending in a letters-only label of two or more characters, an optional :port, then a slash
	// and a path. A reader still resolves this as a link, so it is judged by the same allowlist
	// as a full URL. The token is stripped of a leading protocol-relative "//" and leading or
	// trailing markdown emphasis characters before this pattern is tried; see hasForeignURL.
	bareHostPattern = regexp.MustCompile(`^(?:[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?\.)+[A-Za-z]{2,}(?::\d+)?/\S*$`)
	contextPattern  = regexp.MustCompile(`(?i)userEmail|claudeMd|gitStatus|today's date is|working directory|<system-reminder`)
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

// NewGate compiles cfg, case-insensitively. Deny terms shorter than three characters are ignored.
// A term of five or more characters matches as a substring anywhere, so it also catches an
// identifier built from it, such as a term "acmehq" inside "AcmeHQClient" or "acmehq_billing". A
// shorter term (three or four characters) matches only as a whole word, where the boundary is any
// rune that is not a letter or a digit: notably, unlike a Go identifier boundary, an underscore
// counts as a boundary, so "dev" matches "dev_tools" but not "developer". The shorter, noisier a
// term is, the more it needs a word boundary to avoid matching unrelated words that merely contain it.
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
		n := utf8.RuneCountInString(t)
		if n < minDenyTermLen || seen[key] {
			continue
		}
		seen[key] = true
		if n >= minSubstringDenyTermLen {
			g.deny = append(g.deny, regexp.MustCompile(`(?i)`+regexp.QuoteMeta(t)))
			continue
		}
		g.deny = append(g.deny, regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}])`+regexp.QuoteMeta(t)+`(?:$|[^\p{L}\p{N}])`))
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
	q := normalize(strings.TrimSpace(question))
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

// normalize maps every Unicode space separator (category Zs: NBSP and its relatives) to an ASCII
// space, and drops every Unicode format character (category Cf: zero-width spaces, joiners, and
// other runes with no visible glyph). Without this, a rule built on a normal regex or whitespace
// split can be dodged by hiding a break inside an invisible or non-ASCII space character.
func normalize(q string) string {
	var b strings.Builder
	b.Grow(len(q))
	for _, r := range q {
		switch {
		case unicode.Is(unicode.Cf, r):
			continue
		case unicode.Is(unicode.Zs, r):
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// tokens splits on whitespace and on quoting and bracketing punctuation.
func tokens(q string) []string {
	return strings.FieldsFunc(q, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("\"'`()[]{}<>,;", r)
	})
}

func hasSecret(q string) bool {
	if secretPattern.MatchString(q) || hexSecretPattern.MatchString(q) || credentialPattern.MatchString(q) || hasURLPassword(q) {
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

// hasURLPassword reports a URL whose userinfo carries a password, such as
// scheme://user:pass@host. That is a credential, not merely a link, so it is judged secret rather
// than url, whether or not the host is allowlisted.
func hasURLPassword(q string) bool {
	for _, raw := range urlPattern.FindAllString(q, -1) {
		u, err := url.Parse(strings.TrimRight(raw, ".,:;!?"))
		if err != nil || u.User == nil {
			continue
		}
		if _, ok := u.User.Password(); ok {
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
		// Cut at a zone id (%eth0) or a CIDR suffix (/48), whichever comes first, so
		// net.ParseIP sees only the address.
		if i := strings.IndexAny(t, "/%"); i >= 0 {
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

// hasForeignURL reports a URL whose host is not on the allowlist, or whose userinfo names a user
// with no password (a password is judged secret instead; see hasURLPassword, checked earlier). It
// also treats a scheme-less "host.tld/path" mention as a URL, since a reader still resolves it as
// a link.
func (g *Gate) hasForeignURL(q string) bool {
	for _, raw := range urlPattern.FindAllString(q, -1) {
		u, err := url.Parse(strings.TrimRight(raw, ".,:;!?"))
		if err != nil || u.User != nil || !g.hostAllowed(strings.ToLower(u.Hostname())) {
			return true
		}
	}
	for _, tok := range tokens(q) {
		if strings.Contains(tok, "://") {
			continue
		}
		tok = strings.TrimRight(tok, ".,:;!?")
		// A protocol-relative link ("//host/path") or markdown emphasis around one
		// ("**host/path**") still reads as a link to a person, so strip the decoration before
		// testing the shape. "/" is only stripped from the left (a leading "//"); "*_~" are
		// stripped from both ends.
		tok = strings.TrimLeft(tok, "/*_~")
		tok = strings.TrimRight(tok, "*_~")
		if !bareHostPattern.MatchString(tok) {
			continue
		}
		host := tok
		if i := strings.IndexByte(host, '/'); i >= 0 {
			host = host[:i]
		}
		if i := strings.IndexByte(host, ':'); i >= 0 {
			host = host[:i]
		}
		if !g.hostAllowed(strings.ToLower(host)) {
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
