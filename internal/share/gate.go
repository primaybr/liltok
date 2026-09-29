// Package share turns questions from a user's own cache into candidates for the shared answer pack.
// The rules themselves are pure functions of their input: no database, no network. CurrentDenyEnv
// is the one exception: it reads the OS user and runs git to learn the machine's identity.
package share

import (
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
	RuleSize      = "size"
	RuleSecret    = "secret"
	RulePII       = "pii"
	RulePath      = "path"
	RuleURL       = "url"
	RuleContext   = "context"
	RuleDenyTerm  = "deny_term"
	RuleCode      = "code"
	RuleTemporal  = "temporal"
	RulePayload   = "payload"
	RuleProbe     = "probe"
	RuleAssertion = "assertion"
	RuleError     = "error"
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
	// maxOneWordAnswerChars bounds the probe rule's "answer in one word" shape (see oneWordAnswerPattern)
	// to a short question, so a longer question that merely discusses answering in one word in
	// passing is not caught by it.
	maxOneWordAnswerChars = 80
	// minPayloadSameLineChars is rule payload's same-line threshold: a label line's own trailing
	// content counts as pasted payload once it reaches this many characters; see hasPayload.
	minPayloadSameLineChars = 12
)

var contextPattern = regexp.MustCompile(`(?i)userEmail|claudeMd|gitStatus|today's date is|working directory|<system-reminder`)

// isoTimestampPattern catches an ISO-8601 timestamp (a date, a literal "T", then hour:minute):
// a question naming one is anchored to a specific run and never reusable, so it is judged
// temporal like "today" or "right now" are.
var isoTimestampPattern = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}T\d{2}:\d{2}`)

// assertionPattern rejects a question that opens by asserting its own premise rather than
// asking about it ("Confirm that ..."): that shape states a private fact as settled instead of
// asking a reusable question.
var assertionPattern = regexp.MustCompile(`(?i)^(?:confirm|verify|validate) that\b`)

// rightSuffixPattern is the right-boundary allowance in the deny-term boundary check: a
// lowercase run continuing past a match that is exactly a plural or participle suffix - "s",
// "es", "ed", or "ing" - still counts as a boundary, provided a real boundary follows it too. It
// also accepts a bare "d": a term "widgetgate" against the text "widgetgated" already ends in
// "e", so English past-tense spelling adds only "d" (never a doubled "ed") - the same silent-e
// elision as "gate" -> "gated". It rejects "widgetgates" and "widgetgated" for a term
// "widgetgate", while "Material" (continuation "ial") still passes over a term "Mater". See
// rightBoundaryOK.
var rightSuffixPattern = regexp.MustCompile(`^(?:ing|es|ed|d|s)(?:[^a-z]|$)`)

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
	deny  []denyRule
}

// denyRule pairs a compiled deny-term pattern with whether a match still needs the boundary check
// in code. A short term (three or four characters) embeds its word boundary directly in the regex
// (boundary is false: a plain MatchString is enough). A term of five or more characters is compiled
// as a bare, boundary-free case-insensitive substring search (boundary is true), because RE2 has no
// lookaround to express its boundary rule directly; matchesWithBoundary checks it afterwards.
type denyRule struct {
	re       *regexp.Regexp
	boundary bool
}

// NewGate compiles cfg, case-insensitively. Deny terms shorter than three characters are ignored.
// A term of three or four characters matches only as a whole word, where the boundary is any rune
// that is not a letter or a digit: notably, unlike a Go identifier boundary, an underscore counts
// as a boundary, so "dev" matches "dev_tools" but not "developer".
//
// A term of five or more characters matches as a substring anywhere the occurrence is
// boundary-safe, so it also catches an identifier built from it, such as a term "acmehq" inside
// "AcmeHQClient" or "acmehq_billing" - while still letting a term like "Mater" pass over an
// unrelated word like "Material", because the run of letters continues in lowercase past the match.
// The left side of the occurrence is a boundary unless the rune before it and the first matched rune
// are both lowercase letters (so an uppercase, digit, underscore, or other non-lowercase rune
// immediately before the match - or the very start of the text - is always a boundary, whatever the
// match's own case; only a glued run of lowercase letters on both sides, such as "mywidgetgate", is
// not). The right side is a boundary at the end of the text, at a rune that is not a lowercase
// letter, or - so a plain plural or participle does not read as a different word - at a lowercase
// run continuing past the match that is exactly "s", "es", "ed", or "ing" and is itself followed by
// a boundary. RE2 has no lookaround, so these terms are found with a plain `(?i)`+QuoteMeta(term)
// regex and the boundary runes are checked in code; see matchesWithBoundary, leftBoundaryOK and
// rightBoundaryOK.
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
		quoted := quoteTermWhitespace(t)
		if n >= minSubstringDenyTermLen {
			g.deny = append(g.deny, denyRule{re: regexp.MustCompile(`(?i)` + quoted), boundary: true})
			continue
		}
		g.deny = append(g.deny, denyRule{re: regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}])` + quoted + `(?:$|[^\p{L}\p{N}])`)})
	}
	return g
}

// quoteTermWhitespace escapes a deny term's regex metacharacters word by word and joins the words
// with `\s+`, so a multi-word term still matches when its words are broken across a double space or
// a line break in the question, not only the single literal space the term itself uses.
func quoteTermWhitespace(t string) string {
	fields := strings.Fields(t)
	if len(fields) == 0 {
		return regexp.QuoteMeta(t)
	}
	quoted := make([]string, len(fields))
	for i, f := range fields {
		quoted[i] = regexp.QuoteMeta(f)
	}
	return strings.Join(quoted, `\s+`)
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
	q := strings.TrimSpace(normalize(question))
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
	for _, dr := range g.deny {
		if dr.boundary {
			if matchesWithBoundary(q, dr.re) {
				return Verdict{Rule: RuleDenyTerm}
			}
			continue
		}
		if dr.re.MatchString(q) {
			return Verdict{Rule: RuleDenyTerm}
		}
	}
	if hasCodeDump(q) {
		return Verdict{Rule: RuleCode}
	}
	if isoTimestampPattern.MatchString(q) || !semantic.CheckSemanticEligibility(q).IsEligible {
		return Verdict{Rule: RuleTemporal}
	}
	if hasPayload(q) {
		return Verdict{Rule: RulePayload}
	}
	if hasProbe(q) {
		return Verdict{Rule: RuleProbe}
	}
	if assertionPattern.MatchString(q) {
		return Verdict{Rule: RuleAssertion}
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

// matchesWithBoundary reports whether re has an occurrence in q whose left and right sides are
// both boundary-safe; see NewGate's doc comment for the exact rule. re itself carries no boundary
// (it is a bare `(?i)`+QuoteMeta(term) pattern), so every candidate occurrence is checked here.
func matchesWithBoundary(q string, re *regexp.Regexp) bool {
	for _, loc := range re.FindAllStringIndex(q, -1) {
		if leftBoundaryOK(q, loc[0]) && rightBoundaryOK(q, loc[1]) {
			return true
		}
	}
	return false
}

// leftBoundaryOK is the left-side check of matchesWithBoundary: the start of the text, or any rune
// pair other than "previous rune lowercase and first matched rune (read from the text, not the
// term) also lowercase". unicode.IsLower reports false alike for a letter, a digit, or a CJK
// ideograph, so an acronym or all-caps run before the match ("APIWidgetgate"), a digit before it
// ("v2widgetgate"), and a CJK character before it ("...widgetgate...") are all boundaries
// unconditionally, whatever the first matched rune's case. A glued lowercase run such as
// "mywidgetgate" is the one accepted residual: prev and first are both lowercase, so it is still
// not a boundary.
func leftBoundaryOK(q string, start int) bool {
	if start == 0 {
		return true
	}
	prev, _ := utf8.DecodeLastRuneInString(q[:start])
	first, _ := utf8.DecodeRuneInString(q[start:])
	return !(unicode.IsLower(prev) && unicode.IsLower(first))
}

// rightBoundaryOK is the right-side check of matchesWithBoundary: the end of the text, a rune that
// is not a lowercase letter (an uppercase letter, digit, underscore, hyphen, dot, space or other
// punctuation all count), or a lowercase run continuing past the match that is exactly a plural or
// participle suffix ("s", "es", "ed", "ing") and is itself followed by a boundary - so a plain
// plural or past-tense form of the term still counts as a match. "Material" continues past a match
// on "Mater" with "ial", which is none of those four, so it still passes.
func rightBoundaryOK(q string, end int) bool {
	if end >= len(q) {
		return true
	}
	next, _ := utf8.DecodeRuneInString(q[end:])
	if !unicode.IsLower(next) {
		return true
	}
	return rightSuffixPattern.MatchString(q[end:])
}
