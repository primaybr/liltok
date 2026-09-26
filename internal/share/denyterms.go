package share

import (
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"strings"
	"unicode/utf8"
)

// DenyEnv is the identity of the machine a scan runs on.
type DenyEnv struct {
	Username  string
	Hostname  string
	GitName   string
	GitEmail  string
	RepoOwner string
	RepoName  string
	// RepoExtra holds any path segments between the owner and the repo name in the origin remote
	// URL, such as a GitLab subgroup; see parseRepoURL.
	RepoExtra []string
}

// genericNames are account and host names too common to identify anyone; using them as deny terms
// would reject ordinary questions ("run as root", "bind to localhost").
var genericNames = map[string]bool{
	"root": true, "admin": true, "administrator": true, "user": true, "dev": true, "developer": true,
	"ubuntu": true, "runner": true, "localhost": true, "home": true, "desktop": true, "laptop": true, "pc": true,
}

// genericAccountPattern matches a generic OS account name, optionally followed by whitespace and a
// number ("Windows 11", "win10", "User2"): a login/display name built from one of these identifies
// the machine's OS or role, not the contributor, however it is decorated. It is checked against the
// username only, after the domain-prefix strip already applied in AutoDenyTerms.
var genericAccountPattern = regexp.MustCompile(`(?i)^(?:windows|win|mac|macos|ubuntu|user|admin)\s*\d*$`)

// genericDomains are public mail domains; an address there identifies its owner, its domain does not.
var genericDomains = map[string]bool{
	"gmail.com": true, "googlemail.com": true, "outlook.com": true, "hotmail.com": true, "live.com": true,
	"yahoo.com": true, "icloud.com": true, "me.com": true, "proton.me": true, "protonmail.com": true,
	"users.noreply.github.com": true,
}

// CurrentDenyEnv reads the OS username, hostname, git identity and, from the current directory's
// origin remote, the repository's owner, name and any intermediate subgroup segments. Missing or
// unparsable values stay empty; this reads the environment, unlike every other rule in this
// package, which is a pure function of its input.
func CurrentDenyEnv() DenyEnv {
	var e DenyEnv
	if u, err := user.Current(); err == nil {
		e.Username = u.Username
	}
	e.Hostname, _ = os.Hostname()
	e.GitName = gitConfig("user.name")
	e.GitEmail = gitConfig("user.email")
	e.RepoOwner, e.RepoName, e.RepoExtra = parseRepoURL(gitConfig("remote.origin.url"))
	return e
}

func gitConfig(key string) string {
	out, err := exec.Command("git", "config", "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// parseRepoURL extracts the repository identity from a git remote URL, in either the
// https://host/owner/.../name(.git) form or the SCP-like user@host:[/]owner/.../name(.git) form (any
// "user@host:" prefix is accepted, not only "git@"); a trailing slash is tolerated the same way a
// trailing ".git" is, and an optional leading "/" right after the SCP colon is stripped the same way
// the URL form's leading "/" is. The first path segment is the owner and the last is the repository
// name; anything in between - a GitLab-style subgroup - is returned as extra, in path order. Anything
// that does not resolve to at least two non-empty path segments returns two empty strings and no
// extra segments.
func parseRepoURL(raw string) (owner, name string, extra []string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", nil
	}
	var path string
	if !strings.Contains(raw, "://") {
		at := strings.Index(raw, "@")
		if at < 0 {
			return "", "", nil
		}
		_, p, ok := strings.Cut(raw[at+1:], ":")
		if !ok {
			return "", "", nil
		}
		path = strings.TrimPrefix(p, "/")
	} else {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return "", "", nil
		}
		path = strings.TrimPrefix(u.Path, "/")
	}
	path = strings.TrimSuffix(path, "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return "", "", nil
	}
	for _, p := range parts {
		if p == "" {
			return "", "", nil
		}
	}
	if len(parts) > 2 {
		extra = append([]string(nil), parts[1:len(parts)-1]...)
	}
	return parts[0], parts[len(parts)-1], extra
}

// AutoDenyTerms returns the identity terms to reject questions on: the username without a domain
// prefix, the hostname and its first label, the git name, the git email with its local part and
// domain, and the repository owner and name. Generic names, generic OS account names (the username
// only), and public mail domains are left out; terms shorter than three characters too.
func AutoDenyTerms(e DenyEnv) []string {
	var out []string
	seen := map[string]bool{}
	add := func(t string) {
		t = strings.TrimSpace(t)
		key := strings.ToLower(t)
		if utf8.RuneCountInString(t) < minDenyTermLen || genericNames[key] || genericDomains[key] || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, t)
	}
	u := e.Username
	if i := strings.LastIndexAny(u, `\/`); i >= 0 {
		u = u[i+1:]
	}
	if !genericAccountPattern.MatchString(strings.TrimSpace(u)) {
		add(u)
	}
	host := e.Hostname
	if i := strings.Index(host, "."); i > 0 {
		add(host)
		host = host[:i]
	}
	add(host)
	add(e.GitName)
	if e.GitEmail != "" {
		add(e.GitEmail)
		if i := strings.LastIndex(e.GitEmail, "@"); i >= 0 {
			local := e.GitEmail[:i]
			if utf8.RuneCountInString(local) >= minSubstringDenyTermLen {
				add(local)
			}
			// A GitHub noreply address such as "12345+jdoe@users.noreply.github.com" would otherwise
			// never emit a usable term, since the numeric id dominates the local part's length check
			// and the domain is generic. A "+" in the local part usually separates a numeric id (or
			// a role) from the handle that actually identifies the contributor, so the part before
			// the first "+" and the part after the last "+" are also added as candidates, each
			// subject to the same length and generic-name filters as everything else.
			if j := strings.Index(local, "+"); j >= 0 {
				add(local[:j])
			}
			if j := strings.LastIndex(local, "+"); j >= 0 {
				add(local[j+1:])
			}
			add(e.GitEmail[i+1:])
		}
	}
	add(e.RepoOwner)
	for _, s := range e.RepoExtra {
		add(s)
	}
	add(e.RepoName)
	return out
}
