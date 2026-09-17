package prune

import (
	"bufio"
	"fmt"
	"regexp"
	"strings"
)

var (
	diffHeaderRegex  = regexp.MustCompile(`^diff --git `)
	hunkHeaderRegex  = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+\d+(?:,\d+)? @@`)
	indexHeaderRegex = regexp.MustCompile(`^index [0-9a-fA-F]+\.\.[0-9a-fA-F]+`)
)

// CompactDiff detects unified diff text and compacts unchanged context lines and metadata.
func CompactDiff(input string) string {
	if !strings.Contains(input, "diff --git") && !strings.Contains(input, "@@ -") {
		return input
	}

	scanner := bufio.NewScanner(strings.NewReader(input))
	var out strings.Builder
	var inHunk bool
	var unchangedBuffer []string

	flushUnchanged := func(isHunkStart, isHunkEnd bool) {
		if len(unchangedBuffer) == 0 {
			return
		}

		maxPadding := 2
		if len(unchangedBuffer) <= 3 {
			// Small context, output as-is
			for _, l := range unchangedBuffer {
				out.WriteString(l)
				out.WriteByte('\n')
			}
		} else {
			// Redundant context (> 3 lines)
			if isHunkStart {
				omitted := len(unchangedBuffer) - maxPadding
				placeholder := fmt.Sprintf(" ... [%d lines unchanged]\n", omitted)
				omittedBytes := 0
				for _, l := range unchangedBuffer[:omitted] {
					omittedBytes += len(l) + 1
				}
				if omittedBytes > len(placeholder) {
					out.WriteString(placeholder)
					for _, l := range unchangedBuffer[omitted:] {
						out.WriteString(l)
						out.WriteByte('\n')
					}
				} else {
					for _, l := range unchangedBuffer {
						out.WriteString(l)
						out.WriteByte('\n')
					}
				}
			} else if isHunkEnd {
				omitted := len(unchangedBuffer) - maxPadding
				placeholder := fmt.Sprintf(" ... [%d lines unchanged]\n", omitted)
				omittedBytes := 0
				for _, l := range unchangedBuffer[maxPadding:] {
					omittedBytes += len(l) + 1
				}
				if omittedBytes > len(placeholder) {
					for _, l := range unchangedBuffer[:maxPadding] {
						out.WriteString(l)
						out.WriteByte('\n')
					}
					out.WriteString(placeholder)
				} else {
					for _, l := range unchangedBuffer {
						out.WriteString(l)
						out.WriteByte('\n')
					}
				}
			} else {
				// In between changes: keep head and tail padding
				headPadding := maxPadding
				tailPadding := maxPadding
				if len(unchangedBuffer) > headPadding+tailPadding {
					omitted := len(unchangedBuffer) - (headPadding + tailPadding)
					placeholder := fmt.Sprintf(" ... [%d lines unchanged]\n", omitted)
					omittedBytes := 0
					for _, l := range unchangedBuffer[headPadding : len(unchangedBuffer)-tailPadding] {
						omittedBytes += len(l) + 1
					}
					if omittedBytes > len(placeholder) {
						for _, l := range unchangedBuffer[:headPadding] {
							out.WriteString(l)
							out.WriteByte('\n')
						}
						out.WriteString(placeholder)
						for _, l := range unchangedBuffer[len(unchangedBuffer)-tailPadding:] {
							out.WriteString(l)
							out.WriteByte('\n')
						}
					} else {
						for _, l := range unchangedBuffer {
							out.WriteString(l)
							out.WriteByte('\n')
						}
					}
				} else {
					for _, l := range unchangedBuffer {
						out.WriteString(l)
						out.WriteByte('\n')
					}
				}
			}
		}
		unchangedBuffer = unchangedBuffer[:0]
	}

	firstHunkLine := false

	for scanner.Scan() {
		line := scanner.Text()
		trimmedLine := strings.TrimRight(line, " \t")

		// Strip git index lines like "index 1234567..89abcdef 100644"
		if indexHeaderRegex.MatchString(trimmedLine) {
			continue
		}

		if hunkHeaderRegex.MatchString(trimmedLine) {
			if inHunk {
				flushUnchanged(false, true)
			}
			inHunk = true
			firstHunkLine = true
			out.WriteString(trimmedLine)
			out.WriteByte('\n')
			continue
		}

		if diffHeaderRegex.MatchString(trimmedLine) {
			if inHunk {
				flushUnchanged(false, true)
				inHunk = false
			}
			out.WriteString(trimmedLine)
			out.WriteByte('\n')
			continue
		}

		if inHunk {
			if strings.HasPrefix(trimmedLine, "+") || strings.HasPrefix(trimmedLine, "-") {
				if len(unchangedBuffer) > 0 {
					flushUnchanged(firstHunkLine, false)
				}
				firstHunkLine = false
				out.WriteString(trimmedLine)
				out.WriteByte('\n')
			} else if strings.HasPrefix(trimmedLine, " ") || trimmedLine == "" {
				unchangedBuffer = append(unchangedBuffer, trimmedLine)
			} else {
				// End of hunk or unknown line (e.g. \ No newline at end of file)
				if len(unchangedBuffer) > 0 {
					flushUnchanged(false, true)
				}
				inHunk = false
				out.WriteString(trimmedLine)
				out.WriteByte('\n')
			}
		} else {
			out.WriteString(trimmedLine)
			out.WriteByte('\n')
		}
	}

	if inHunk && len(unchangedBuffer) > 0 {
		flushUnchanged(false, true)
	}

	res := out.String()
	if !strings.HasSuffix(input, "\n") && strings.HasSuffix(res, "\n") {
		res = strings.TrimSuffix(res, "\n")
	}
	return res
}
