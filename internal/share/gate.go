// Package share turns questions from a user's own cache into candidates for the shared answer pack.
// The rules themselves are pure functions of their input: no database, no network. CurrentDenyEnv
// is the one exception: it reads the OS user and runs git to learn the machine's identity.
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

var (
	// secretPattern's final alternative catches a dashed UUID (8-4-4-4-12 hex groups): a run or
	// request ID naming one is not itself a secret token, but it always identifies one specific
	// private run, so it is judged secret rather than let through as prose.
	secretPattern = regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{8,}|\bsk-[A-Za-z0-9_\-]{16,}|\bgsk_[A-Za-z0-9]{16,}|\bnvapi-[A-Za-z0-9_\-]{16,}|\bgh[pousr]_[A-Za-z0-9]{20,}|\bgithub_pat_[A-Za-z0-9_]{20,}|\bAIza[0-9A-Za-z_\-]{30,}|\bAKIA[0-9A-Z]{16}\b|\bxox[abp]-[A-Za-z0-9\-]{10,}|\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}|-----BEGIN [A-Z ]*PRIVATE KEY-----|(?i:\bbearer\s+[A-Za-z0-9._~+/\-]{16,})|\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	// hexSecretPattern catches a long hex-only run (a hex-encoded key or a UUID used as one):
	// Shannon entropy tops out at exactly 4.0 for a 16-symbol alphabet, so it can never exceed
	// minTokenEntropy and needs its own check.
	hexSecretPattern = regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`)
	// credentialPattern catches a plain "keyword = value" or "keyword: value" credential that has
	// no distinctive shape of its own, such as password=Summer2024!. The leading boundary is
	// "not a letter or digit" rather than \b, so an underscore still counts as a boundary (an
	// env-var name like DB_PASSWORD or GITHUB_TOKEN triggers); an optional closing quote is
	// allowed between the keyword and the separator, so a quoted JSON key like "password": ...
	// also triggers. The keyword may carry any number of "_"/"-"-joined word suffixes
	// (STRIPE_SECRET_KEY, DB_PASS_PROD), so a short or otherwise unremarkable value after it is
	// still judged a credential; a bare-letter suffix with no separator is not accepted, so
	// max_tokens: 4096 is not "token" plus a suffix. A lowercase letter immediately before a
	// capitalized keyword (dbPassword, clientSecret, apiToken) is also a boundary, for a camelCase
	// name that the non-alphanumeric boundary alone would miss.
	credentialPattern = regexp.MustCompile(`(?:(?i:(?:^|[^A-Za-z0-9])(?:pass(?:word|wd)?|pwd|secret|api[_-]?key|access[_-]?token|auth[_-]?token|token))|[a-z](?:Pass(?:word|wd)?|Pwd|Secret|Api[_-]?Key|Access[_-]?Token|Auth[_-]?Token|Token))(?:[_-][A-Za-z0-9]+)*["']?\s*[:=]\s*\S+`)
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
	// privateHostSuffixPattern matches a scheme-less, path-less hostname whose final label is a
	// private-network suffix (internal, corp, local, lan, intranet): a reader still recognizes this
	// as a private host name even with no path to resolve. "arpa" (reverse-DNS infrastructure, as in
	// in-addr.arpa) is deliberately excluded, since it names public resolver zones, not a private
	// host.
	privateHostSuffixPattern = regexp.MustCompile(`(?i)^(?:[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?\.)+(?:internal|corp|local|lan|intranet)$`)
	contextPattern           = regexp.MustCompile(`(?i)userEmail|claudeMd|gitStatus|today's date is|working directory|<system-reminder`)
	// isoTimestampPattern catches an ISO-8601 timestamp (a date, a literal "T", then hour:minute):
	// a question naming one is anchored to a specific run and never reusable, so it is judged
	// temporal like "today" or "right now" are.
	isoTimestampPattern = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}T\d{2}:\d{2}`)
	// payloadLabelPattern is rule payload's label-line shape: at line start, an optional markdown
	// heading marker, an optional opening "**", one of the listed "paste your X below" template
	// labels, an optional closing "**", a colon (ASCII or the fullwidth "：" a CJK input method can
	// produce), and another optional closing "**" (a bold wrapper can close either before or after
	// the colon: "**Text**:" or "**Text:**"). It matches only this prefix - what follows is judged
	// separately by hasPayload, so "Content-Type: text/plain" never matches: "content" is a label,
	// but "-Type" sits directly between it and the colon, and nothing here allows that gap.
	payloadLabelPattern = regexp.MustCompile(`(?i)^\s*(?:#{1,6}\s*)?(?:\*\*)?(?:here is the log|here is the text|here is the input|here are the notes|here is the transcript|session log|session notes|text|input|content|logs|log|transcript|document|data|notes|paste)(?:\*\*)?(?::|：)(?:\*\*)?`)
	// tagWrappedPayloadPattern is rule payload's tag-wrapped shape: an opening tag such as "<text>"
	// or "<log>" around pasted material. Extraction already drops unknown tags on its own; this is
	// defense in depth for a tag it does recognize.
	tagWrappedPayloadPattern = regexp.MustCompile(`(?i)<(?:text|input|log|document|transcript|notes|content)>`)
	// bracketTimestampPattern is rule payload's bracketed-timestamp shape: a timestamp in brackets
	// at line start, such as "[09:12]" or "[09:12:33]" - the shape a pasted chat or log line keeps
	// once it is copied into a question.
	bracketTimestampPattern = regexp.MustCompile(`(?m)^\[\d{1,2}:\d{2}(?::\d{2})?\]`)
	// timestampHeadingPattern is rule payload's markdown-heading shape: a heading that opens with a
	// clock-style timestamp, such as "## 14:05 | develop" - the heading a pasted log excerpt keeps
	// once it is copied into a question. The separator after the time is a plain word boundary
	// rather than a specific "-"/"|"/end-of-line, and an optional ":SS" seconds group is allowed, so
	// "## 09:12:33 main" and "## 09:12 main" (no separator at all after the time) both match.
	timestampHeadingPattern = regexp.MustCompile(`(?m)^#{1,6}\s*\d{1,2}:\d{2}(?::\d{2})?\b`)
	// probePattern rejects a liveness or echo prompt: a question that asks for a fixed, literal
	// reply rather than an explanation. Matching any one alternative is enough on its own: alongside
	// the direct "reply exactly" shapes, it includes a role-play opener ("You are ...") that primes
	// a persona rather than asking a question, and a trailing "token"/"nonce" plus a short hex value,
	// the shape a liveness probe uses to check that a specific reply comes back verbatim.
	probePattern = regexp.MustCompile(`(?i)^(?:reply|respond|answer)\b.{0,40}\b(?:exactly|only)\b|^say \S+ in (?:one|1|exactly one|exactly 1) word|\brespond with status ok\b|^(?:what is|calculate) [\d\s+\-*/().]+\??.{0,40}\b(?:just|only) the number|^you are (?:in|a|an|the|summari[sz]ing)\b|\b(?:token|nonce)\s+[0-9a-f]{4,12}\.?$`)
	// oneWordAnswerPattern is one of probe's shapes: a request for a single-word answer. It is
	// judged only on a short question (see hasProbe's length check) so a longer question that
	// merely discusses "answer in one word" in passing - about ICU collation, say - is not caught.
	oneWordAnswerPattern = regexp.MustCompile(`(?i)\banswer in (?:one|1|a single) word\.?$`)
	// assertionPattern rejects a question that opens by asserting its own premise rather than
	// asking about it ("Confirm that ..."): that shape states a private fact as settled instead of
	// asking a reusable question.
	assertionPattern = regexp.MustCompile(`(?i)^(?:confirm|verify|validate) that\b`)
	// lowerWordPattern is one segment of the path rule's "ordinary English words joined by slashes"
	// exemption: see isExemptOrdinaryWords.
	lowerWordPattern = regexp.MustCompile(`^[a-z]{1,8}$`)
	// wholeTokenLowerAlnumPath is the shape path rule exemption (a) requires of the whole token (or,
	// for a golang.org/x/ path, of the part after the prefix): one or more lowercase-alphanumeric
	// segments joined by "/", with no dots, underscores, uppercase letters or file extensions
	// anywhere in it. Checking the whole token, not just its first segment, keeps a real path under
	// a stdlib-named directory (such as "go/pkg/mod/github.com/secretco/billing/tax.go") from
	// passing as an import path.
	wholeTokenLowerAlnumPath = regexp.MustCompile(`^[a-z0-9]+(?:/[a-z0-9]+)*$`)
	// trailingCapIdentPattern strips one trailing ".Identifier" whose first letter is uppercase, such
	// as ".ReverseProxy" in "net/http/httputil.ReverseProxy", before exemption (a)'s whole-token
	// check; see isExemptPathToken. That is a package-qualified exported Go identifier tacked onto
	// an import path, not a filesystem path segment. A trailing lowercase extension such as ".go" is
	// left alone and still falls through to rejection. The pattern also requires at least one
	// lowercase letter in the identifier, so an all-uppercase suffix such as ".YAML" - itself a file
	// extension in disguise, not an exported Go identifier, which is almost always mixed-case - is
	// not stripped, and the token still reads as a real nested path.
	trailingCapIdentPattern = regexp.MustCompile(`\.[A-Z][A-Za-z0-9_]*[a-z][A-Za-z0-9_]*$`)
	// rightSuffixPattern is the right-boundary allowance in the deny-term boundary check: a
	// lowercase run continuing past a match that is exactly a plural or participle suffix - "s",
	// "es", "ed", or "ing" - still counts as a boundary, provided a real boundary follows it too. It
	// also accepts a bare "d": a term "widgetgate" against the text "widgetgated" already ends in
	// "e", so English past-tense spelling adds only "d" (never a doubled "ed") - the same silent-e
	// elision as "gate" -> "gated". It rejects "widgetgates" and "widgetgated" for a term
	// "widgetgate", while "Material" (continuation "ial") still passes over a term "Mater". See
	// rightBoundaryOK.
	rightSuffixPattern = regexp.MustCompile(`^(?:ing|es|ed|d|s)(?:[^a-z]|$)`)
)

// commonDirNames are directory names common enough in a real relative path that a slash-joined run
// containing one should not be exempted as "ordinary English words" (path rule exemption (b)):
// without this list, a real path like "home/alice/secret/data" or "opt/billing/tax/rates" would
// read as prose. "mod" is included because the go/pkg/mod/... module cache layout would otherwise
// let a token like "go/pkg/mod/secretco/billing.Config" still read as an import path merely because
// "go" is itself a real stdlib top-level package name; see isExemptPathToken's stdlib branch.
var commonDirNames = map[string]bool{
	"home": true, "users": true, "user": true, "var": true, "etc": true, "tmp": true, "usr": true,
	"opt": true, "srv": true, "mnt": true, "src": true, "app": true, "lib": true, "bin": true,
	"cmd": true, "pkg": true, "internal": true, "root": true, "data": true, "config": true, "mod": true,
}

// stdlibTopLevel is the set of Go standard-library top-level import segments; a token whose first
// slash-separated segment is one of these (and has no dot, so it is not a domain) reads as an
// import path, not a filesystem path. See isExemptPathToken.
var stdlibTopLevel = map[string]bool{
	"archive": true, "bufio": true, "bytes": true, "compress": true, "container": true, "context": true,
	"crypto": true, "database": true, "debug": true, "embed": true, "encoding": true, "errors": true,
	"expvar": true, "flag": true, "fmt": true, "go": true, "hash": true, "html": true, "image": true,
	"index": true, "io": true, "iter": true, "log": true, "maps": true, "math": true, "mime": true,
	"net": true, "os": true, "path": true, "plugin": true, "reflect": true, "regexp": true, "runtime": true,
	"slices": true, "sort": true, "strconv": true, "strings": true, "structs": true, "sync": true,
	"syscall": true, "testing": true, "text": true, "time": true, "unicode": true, "unique": true, "unsafe": true,
}

// golangOrgXPrefix is the golang.org/x/... module namespace: an import path from it never has a
// stdlib-style first segment (golang.org has a dot), so it needs its own exemption in both the path
// rule and the bare-host check of the url rule.
const golangOrgXPrefix = "golang.org/x/"

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

// hasPayload reports a question built from a "paste your X below" template whose payload was
// pasted in. It checks four independent shapes, any one of which is enough: a markdown heading or
// a bracketed timestamp that opens a copied log line (timestampHeadingPattern,
// bracketTimestampPattern); an opening tag such as "<text>" wrapping pasted material
// (tagWrappedPayloadPattern); a Python-style triple-quoted block (hasTripleQuotedPayload); or a
// label line (payloadLabelPattern) - fired when either at least minPayloadSameLineChars of
// non-empty content follows the colon on the same line, or a later line is non-empty.
func hasPayload(q string) bool {
	if timestampHeadingPattern.MatchString(q) || bracketTimestampPattern.MatchString(q) ||
		tagWrappedPayloadPattern.MatchString(q) || hasTripleQuotedPayload(q) {
		return true
	}
	lines := strings.Split(q, "\n")
	for i, line := range lines {
		loc := payloadLabelPattern.FindStringIndex(line)
		if loc == nil {
			continue
		}
		if utf8.RuneCountInString(strings.TrimSpace(line[loc[1]:])) >= minPayloadSameLineChars {
			return true
		}
		for _, later := range lines[i+1:] {
			if strings.TrimSpace(later) != "" {
				return true
			}
		}
	}
	return false
}

// hasTripleQuotedPayload reports a question containing two triple-double-quote or two
// triple-single-quote delimiters with non-empty content between them - a Python-style
// triple-quoted block pasted into a question.
func hasTripleQuotedPayload(q string) bool {
	return hasDelimitedPayload(q, `"""`) || hasDelimitedPayload(q, `'''`)
}

// hasDelimitedPayload reports whether q contains two occurrences of delim with non-empty (once
// trimmed) content between the first pair found.
func hasDelimitedPayload(q, delim string) bool {
	first := strings.Index(q, delim)
	if first < 0 {
		return false
	}
	rest := q[first+len(delim):]
	second := strings.Index(rest, delim)
	if second < 0 {
		return false
	}
	return strings.TrimSpace(rest[:second]) != ""
}

// hasProbe reports a liveness or echo prompt; see probePattern for most of its shapes.
// oneWordAnswerPattern is checked separately because it applies only to a short question: a longer
// one that merely discusses answering in one word, rather than demanding it, should still pass.
func hasProbe(q string) bool {
	if probePattern.MatchString(q) {
		return true
	}
	return oneWordAnswerPattern.MatchString(q) && utf8.RuneCountInString(q) <= maxOneWordAnswerChars
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
			if segments < 3 {
				continue
			}
			if sep == "/" && isExemptPathToken(tok) {
				continue
			}
			return true
		}
	}
	return false
}

// isExemptPathToken reports whether a "/"-separated token with three or more segments should not
// count as a path after all: a Go standard-library import path or a golang.org/x/... module path
// (a), or an ordinary run of short lowercase English words joined by slashes, such as "a/the/an"
// (b). Neither shape is a filesystem or URL path a reader could act on.
func isExemptPathToken(tok string) bool {
	// Exemption (a) requires the whole token (or, for a golang.org/x/ path, the part after the
	// prefix) to be nothing but lowercase-alphanumeric segments, not just its first segment: anything
	// else - a dot, an underscore, an uppercase letter, ".." - falls through to exemption (b) or
	// rejection instead. This keeps a real path nested under a stdlib-named directory
	// ("go/pkg/mod/github.com/secretco/billing/tax.go", "path/to/Secret/Config.yaml") from passing.
	// Before that check, a trailing ".Identifier" whose first letter is uppercase - a
	// package-qualified exported Go identifier such as ".ReverseProxy" in
	// "net/http/httputil.ReverseProxy" - is stripped first, so an import path mentioned together
	// with one of its own exported names still reads as an import path. A trailing lowercase
	// extension such as ".go" is left alone; see stripTrailingCapIdent.
	//
	// The stdlib branch also rejects a token where any segment after the first names a common
	// relative-path directory (see commonDirNames and segsContainCommonDir), since otherwise
	// "go/pkg/mod/secretco/billing.Config" would still read as an import path merely because "go" is
	// itself a real stdlib top-level package name. A token such as "os/secretco/billing.Tax" - where
	// the second segment is not itself a common directory name - is an accepted residual gap:
	// catching every implausible stdlib subpackage would need a real list of actual stdlib import
	// paths, which this rule does not have.
	if rest, ok := strings.CutPrefix(tok, golangOrgXPrefix); ok {
		return wholeTokenLowerAlnumPath.MatchString(stripTrailingCapIdent(rest))
	}
	segs := strings.Split(tok, "/")
	if first := segs[0]; !strings.Contains(first, ".") && stdlibTopLevel[first] &&
		!segsContainCommonDir(segs[1:]) && wholeTokenLowerAlnumPath.MatchString(stripTrailingCapIdent(tok)) {
		return true
	}
	return isExemptOrdinaryWords(tok, segs)
}

// stripTrailingCapIdent removes one trailing ".Identifier" whose first letter is uppercase from s,
// for path exemption (a)'s whole-token check; see isExemptPathToken. s is returned unchanged when
// no such suffix is present.
func stripTrailingCapIdent(s string) string {
	return trailingCapIdentPattern.ReplaceAllString(s, "")
}

// segsContainCommonDir reports whether any of segs names a common relative-path directory (see
// commonDirNames); used by path exemption (a)'s stdlib branch. See isExemptPathToken.
func segsContainCommonDir(segs []string) bool {
	for _, s := range segs {
		if commonDirNames[s] {
			return true
		}
	}
	return false
}

// isExemptOrdinaryWords is path rule exemption (b): a short run of ordinary lowercase English words
// joined by slashes, such as "a/the/an", is prose, not a path. It allows at most 3 segments, and
// never when a segment names a common directory (see commonDirNames), since a real path is far
// likelier to use one of those than a sentence is - so a real relative path like
// "home/alice/secret/data" or "opt/billing/tax/rates" does not pass as prose. A residual gap remains
// for a plausible bare project path like "acme/billing/tax": that still passes as ordinary words, so
// catching a real project name here depends on it also being configured as a deny term.
func isExemptOrdinaryWords(tok string, segs []string) bool {
	if strings.HasPrefix(tok, "/") || strings.HasSuffix(tok, "/") || len(segs) < 2 || len(segs) > 3 {
		return false
	}
	for _, s := range segs {
		if !lowerWordPattern.MatchString(s) || commonDirNames[s] {
			return false
		}
	}
	return true
}

// hasForeignURL reports a URL whose host is not on the allowlist, or whose userinfo names a user
// with no password (a password is judged secret instead; see hasURLPassword, checked earlier). It
// also treats a scheme-less "host.tld/path" mention as a URL, since a reader still resolves it as
// a link, and a scheme-less hostname ending in a private-network suffix even with no path (see
// privateHostSuffixPattern). A loopback host (localhost, 127.0.0.1, or the IPv6 loopback) is exempt
// either way, the same way a bare loopback address is already exempt from the pii rule.
func (g *Gate) hasForeignURL(q string) bool {
	for _, raw := range urlPattern.FindAllString(q, -1) {
		u, err := url.Parse(strings.TrimRight(raw, ".,:;!?"))
		if err != nil {
			return true
		}
		host := strings.ToLower(u.Hostname())
		if isLoopbackHost(host) {
			continue
		}
		if u.User != nil || !g.hostAllowed(host) {
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
		if strings.HasPrefix(tok, golangOrgXPrefix) {
			continue
		}
		if !bareHostPattern.MatchString(tok) && !privateHostSuffixPattern.MatchString(tok) {
			continue
		}
		host := tok
		if i := strings.IndexByte(host, '/'); i >= 0 {
			host = host[:i]
		}
		if i := strings.IndexByte(host, ':'); i >= 0 {
			host = host[:i]
		}
		host = strings.ToLower(host)
		if isLoopbackHost(host) {
			continue
		}
		if !g.hostAllowed(host) {
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

// isLoopbackHost reports whether host (already lowercased) names the local machine: "localhost", or
// an IP address that net.IP.IsLoopback reports true for (127.0.0.0/8, or the IPv6 "::1").
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
