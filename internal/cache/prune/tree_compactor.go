package prune

import (
	"bufio"
	"sort"
	"strings"
	"unicode"
)

// IsTreeLine checks if a line looks like an ASCII/Unicode directory tree line.
func isTreeLine(line string) bool {
	return strings.Contains(line, "├──") ||
		strings.Contains(line, "└──") ||
		strings.Contains(line, "│") ||
		strings.Contains(line, "+--") ||
		strings.Contains(line, "\\--")
}

type treeNode struct {
	depth int
	name  string
}

// CompactTree detects ASCII / Markdown directory tree representations and collapses them into concise bracketed paths.
func CompactTree(input string) string {
	if !strings.Contains(input, "├──") && !strings.Contains(input, "└──") && !strings.Contains(input, "+--") && !strings.Contains(input, "\\--") {
		return input
	}

	scanner := bufio.NewScanner(strings.NewReader(input))
	var out strings.Builder
	var treeLines []string

	flushTree := func() {
		if len(treeLines) == 0 {
			return
		}
		if len(treeLines) < 2 {
			for _, l := range treeLines {
				out.WriteString(l)
				out.WriteByte('\n')
			}
			treeLines = treeLines[:0]
			return
		}

		paths := parseTreeToPaths(treeLines)
		if len(paths) == 0 {
			for _, l := range treeLines {
				out.WriteString(l)
				out.WriteByte('\n')
			}
			treeLines = treeLines[:0]
			return
		}

		compacted := collapsePaths(paths)
		for _, p := range compacted {
			out.WriteString(p)
			out.WriteByte('\n')
		}
		treeLines = treeLines[:0]
	}

	for scanner.Scan() {
		line := scanner.Text()
		if isTreeLine(line) {
			treeLines = append(treeLines, line)
		} else {
			if len(treeLines) > 0 {
				flushTree()
			}
			out.WriteString(line)
			out.WriteByte('\n')
		}
	}
	if len(treeLines) > 0 {
		flushTree()
	}

	res := out.String()
	if !strings.HasSuffix(input, "\n") && strings.HasSuffix(res, "\n") {
		res = strings.TrimSuffix(res, "\n")
	}
	return res
}

func parseTreeToPaths(lines []string) []string {
	var nodes []treeNode
	for _, rawLine := range lines {
		// Calculate depth based on position of tree branch indicator
		var branchIdx int
		var branchLen int
		if idx := strings.Index(rawLine, "├──"); idx != -1 {
			branchIdx, branchLen = idx, 9 // UTF-8 3 bytes each
		} else if idx := strings.Index(rawLine, "└──"); idx != -1 {
			branchIdx, branchLen = idx, 9
		} else if idx := strings.Index(rawLine, "+--"); idx != -1 {
			branchIdx, branchLen = idx, 3
		} else if idx := strings.Index(rawLine, "\\--"); idx != -1 {
			branchIdx, branchLen = idx, 3
		} else {
			continue
		}

		name := strings.TrimSpace(rawLine[branchIdx+branchLen:])
		name = strings.TrimRightFunc(name, unicode.IsSpace)
		if name == "" {
			continue
		}

		depth := branchIdx / 4 // 4 characters per indentation level typically
		nodes = append(nodes, treeNode{depth: depth, name: name})
	}

	if len(nodes) == 0 {
		return nil
	}

	var paths []string
	var stack []string

	for _, n := range nodes {
		if n.depth < len(stack) {
			stack = stack[:n.depth]
		}
		cleanName := strings.TrimSuffix(n.name, "/")
		isDir := strings.HasSuffix(n.name, "/")

		if isDir {
			stack = append(stack, cleanName)
		} else {
			fullPath := append(append([]string{}, stack...), cleanName)
			paths = append(paths, strings.Join(fullPath, "/"))
		}
	}

	return paths
}

func collapsePaths(paths []string) []string {
	// Group paths by directory
	dirGroups := make(map[string][]string)
	for _, p := range paths {
		idx := strings.LastIndex(p, "/")
		if idx == -1 {
			dirGroups[""] = append(dirGroups[""] , p)
		} else {
			dir := p[:idx]
			file := p[idx+1:]
			dirGroups[dir] = append(dirGroups[dir], file)
		}
	}

	// Sort directories
	var dirs []string
	for d := range dirGroups {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	var result []string
	for _, d := range dirs {
		files := dirGroups[d]
		sort.Strings(files)
		if d == "" {
			result = append(result, files...)
			continue
		}
		if len(files) == 1 {
			result = append(result, d+"/"+files[0])
		} else {
			result = append(result, d+"/{" + strings.Join(files, ", ") + "}")
		}
	}

	return result
}
