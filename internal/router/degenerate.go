package router

import (
	"errors"
	"unicode"
	"unicode/utf8"
)

// A reply degenerates when a model stops producing words and emits a long run of punctuation,
// digits and blank lines instead, often until max_tokens. Such a reply has visible text, so the
// empty-completion check passes it, and it repeats nothing, so the repetition check passes it too.

// minWordRunes is the length of the shortest run of letters or digits counted as a word; shorter
// runs ("a", "11") also occur inside symbol noise.
const minWordRunes = 3

// minDegenerateRunes is the number of non-space runes a reply's trailing wordless run must reach
// before the reply is judged degenerate. A markdown table rule or a "=====" banner stays well
// below it.
const minDegenerateRunes = 120

// A wordless run that long is still dense code, such as a regex answer, unless it is mostly line
// breaks (at least one per noiseRunesPerLine non-space runes) or uses at most maxNoiseDistinct
// distinct non-space runes ("!!!!", "....").
const (
	noiseRunesPerLine = 8
	maxNoiseDistinct  = 4
)

// maxWordlessBytes bounds a trailing wordless run of any content, so a model emitting only blank
// lines or spaces is caught too.
const maxWordlessBytes = 2000

// errDegenerateReply marks a reply rejected by degenerateCut.
var errDegenerateReply = errors.New("degenerated into symbol noise")

// wordlessTail returns the byte offset where text's trailing wordless run starts: the text after
// its last word, or 0 when it has none.
func wordlessTail(text string) (start int) {
	streak, streakEnd := 0, 0
	for i := len(text); i > 0; {
		r, size := utf8.DecodeLastRuneInString(text[:i])
		i -= size
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			streak = 0
			continue
		}
		if streak == 0 {
			streakEnd = i + size
		}
		if streak++; streak >= minWordRunes {
			return streakEnd
		}
	}
	return 0
}

// degenerateCut reports whether text ends in a degenerate wordless run, and where that run starts:
// the text before it is the part of the reply still worth keeping.
func degenerateCut(text string) (int, bool) {
	start := wordlessTail(text)
	if len(text)-start >= maxWordlessBytes {
		return start, true
	}
	nonSpace, lines := 0, 0
	distinct := map[rune]bool{}
	for _, r := range text[start:] {
		switch {
		case r == '\n':
			lines++
		case !unicode.IsSpace(r):
			nonSpace++
			distinct[r] = true
		}
	}
	if nonSpace >= minDegenerateRunes && (lines*noiseRunesPerLine >= nonSpace || len(distinct) <= maxNoiseDistinct) {
		return start, true
	}
	return 0, false
}
