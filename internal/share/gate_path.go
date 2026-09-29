package share

import (
	"regexp"
	"strings"
)

var drivePath = regexp.MustCompile(`\b[A-Za-z]:[\\/]`)

var homePath = regexp.MustCompile(`(?i)(?:^|[^\w.])(?:/home/|/users/|~/|~\\)`)

var uncPath = regexp.MustCompile(`(?:^|[^\\])\\\\[A-Za-z0-9._$\-]+\\`)

// lowerWordPattern is one segment of the path rule's "ordinary English words joined by slashes"
// exemption: see isExemptOrdinaryWords.
var lowerWordPattern = regexp.MustCompile(`^[a-z]{1,8}$`)

// wholeTokenLowerAlnumPath is the shape path rule exemption (a) requires of the whole token (or,
// for a golang.org/x/ path, of the part after the prefix): one or more lowercase-alphanumeric
// segments joined by "/", with no dots, underscores, uppercase letters or file extensions
// anywhere in it. Checking the whole token, not just its first segment, keeps a real path under
// a stdlib-named directory (such as "go/pkg/mod/github.com/secretco/billing/tax.go") from
// passing as an import path.
var wholeTokenLowerAlnumPath = regexp.MustCompile(`^[a-z0-9]+(?:/[a-z0-9]+)*$`)

// trailingCapIdentPattern strips one trailing ".Identifier" whose first letter is uppercase, such
// as ".ReverseProxy" in "net/http/httputil.ReverseProxy", before exemption (a)'s whole-token
// check; see isExemptPathToken. That is a package-qualified exported Go identifier tacked onto
// an import path, not a filesystem path segment. A trailing lowercase extension such as ".go" is
// left alone and still falls through to rejection. The pattern also requires at least one
// lowercase letter in the identifier, so an all-uppercase suffix such as ".YAML" - itself a file
// extension in disguise, not an exported Go identifier, which is almost always mixed-case - is
// not stripped, and the token still reads as a real nested path.
var trailingCapIdentPattern = regexp.MustCompile(`\.[A-Z][A-Za-z0-9_]*[a-z][A-Za-z0-9_]*$`)

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
