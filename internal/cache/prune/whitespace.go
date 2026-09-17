package prune

import (
	"regexp"
	"strings"
)

var (
	// Matches repetitive comment divider lines like // ========, /* -------- */, # #########, etc.
	commentDividerRegex = regexp.MustCompile(`(?m)^[ \t]*(?://|/\*|#|--)[ \t]*([=\-_*#~]{4,})[ \t]*(?:\*/)?$`)
	// Matches 3 or more consecutive newlines (i.e. 2 or more empty lines)
	multipleNewlinesRegex = regexp.MustCompile(`\n{3,}`)
	// Matches trailing whitespace on lines
	trailingWhitespaceRegex = regexp.MustCompile(`[ \t]+$`)
)

// StripWhitespaceAndDividers cleans up redundant divider comments and excessive empty lines.
func StripWhitespaceAndDividers(input string) string {
	if len(input) == 0 {
		return input
	}

	// 1. Remove divider comment lines
	cleaned := commentDividerRegex.ReplaceAllString(input, "")

	// 2. Strip trailing whitespace from lines
	lines := strings.Split(cleaned, "\n")
	for i, line := range lines {
		lines[i] = trailingWhitespaceRegex.ReplaceAllString(line, "")
	}
	cleaned = strings.Join(lines, "\n")

	// 3. Collapse 3 or more newlines into at most 2 newlines (at most 1 blank line)
	cleaned = multipleNewlinesRegex.ReplaceAllString(cleaned, "\n\n")

	return cleaned
}
