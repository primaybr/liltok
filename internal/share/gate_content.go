package share

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// payloadLabelPattern is rule payload's label-line shape: at line start, an optional markdown
// heading marker, an optional opening "**", one of the listed "paste your X below" template
// labels, an optional closing "**", a colon (ASCII or the fullwidth "：" a CJK input method can
// produce), and another optional closing "**" (a bold wrapper can close either before or after
// the colon: "**Text**:" or "**Text:**"). It matches only this prefix - what follows is judged
// separately by hasPayload, so "Content-Type: text/plain" never matches: "content" is a label,
// but "-Type" sits directly between it and the colon, and nothing here allows that gap.
var payloadLabelPattern = regexp.MustCompile(`(?i)^\s*(?:#{1,6}\s*)?(?:\*\*)?(?:here is the log|here is the text|here is the input|here are the notes|here is the transcript|session log|session notes|text|input|content|logs|log|transcript|document|data|notes|paste)(?:\*\*)?(?::|：)(?:\*\*)?`)

// tagWrappedPayloadPattern is rule payload's tag-wrapped shape: an opening tag such as "<text>"
// or "<log>" around pasted material. Extraction already drops unknown tags on its own; this is
// defense in depth for a tag it does recognize.
var tagWrappedPayloadPattern = regexp.MustCompile(`(?i)<(?:text|input|log|document|transcript|notes|content)>`)

// bracketTimestampPattern is rule payload's bracketed-timestamp shape: a timestamp in brackets
// at line start, such as "[09:12]" or "[09:12:33]" - the shape a pasted chat or log line keeps
// once it is copied into a question.
var bracketTimestampPattern = regexp.MustCompile(`(?m)^\[\d{1,2}:\d{2}(?::\d{2})?\]`)

// timestampHeadingPattern is rule payload's markdown-heading shape: a heading that opens with a
// clock-style timestamp, such as "## 14:05 | develop" - the heading a pasted log excerpt keeps
// once it is copied into a question. The separator after the time is a plain word boundary
// rather than a specific "-"/"|"/end-of-line, and an optional ":SS" seconds group is allowed, so
// "## 09:12:33 main" and "## 09:12 main" (no separator at all after the time) both match.
var timestampHeadingPattern = regexp.MustCompile(`(?m)^#{1,6}\s*\d{1,2}:\d{2}(?::\d{2})?\b`)

// probePattern rejects a liveness or echo prompt: a question that asks for a fixed, literal
// reply rather than an explanation. Matching any one alternative is enough on its own: alongside
// the direct "reply exactly" shapes, it includes a role-play opener ("You are ...") that primes
// a persona rather than asking a question, and a trailing "token"/"nonce" plus a short hex value,
// the shape a liveness probe uses to check that a specific reply comes back verbatim.
var probePattern = regexp.MustCompile(`(?i)^(?:reply|respond|answer)\b.{0,40}\b(?:exactly|only)\b|^say \S+ in (?:one|1|exactly one|exactly 1) word|\brespond with status ok\b|^(?:what is|calculate) [\d\s+\-*/().]+\??.{0,40}\b(?:just|only) the number|^you are (?:in|a|an|the|summari[sz]ing)\b|\b(?:token|nonce)\s+[0-9a-f]{4,12}\.?$`)

// oneWordAnswerPattern is one of probe's shapes: a request for a single-word answer. It is
// judged only on a short question (see hasProbe's length check) so a longer question that
// merely discusses "answer in one word" in passing - about ICU collation, say - is not caught.
var oneWordAnswerPattern = regexp.MustCompile(`(?i)\banswer in (?:one|1|a single) word\.?$`)

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
