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
// origin remote, the repository's owner and name. Missing or unparsable values stay empty.
func CurrentDenyEnv() DenyEnv {
	var e DenyEnv
	if u, err := user.Current(); err == nil {
		e.Username = u.Username
	}
	e.Hostname, _ = os.Hostname()
	e.GitName = gitConfig("user.name")
	e.GitEmail = gitConfig("user.email")
	e.RepoOwner, e.RepoName = parseRepoURL(gitConfig("remote.origin.url"))
	return e
}

func gitConfig(key string) string {
	out, err := exec.Command("git", "config", "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// parseRepoURL extracts the owner and repository name from a git remote URL, in either the
// https://host/owner/name(.git) form or the SCP-like git@host:owner/name(.git) form; a trailing
// slash is tolerated the same way a trailing ".git" is. Anything else, including a URL that does
// not resolve to exactly two path segments, returns two empty strings.
func parseRepoURL(raw string) (owner, name string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	var path string
	if rest, ok := strings.CutPrefix(raw, "git@"); ok {
		_, p, ok := strings.Cut(rest, ":")
		if !ok {
			return "", ""
		}
		path = p
	} else {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return "", ""
		}
		path = strings.TrimPrefix(u.Path, "/")
	}
	path = strings.TrimSuffix(path, "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", ""
	}
	return parts[0], parts[1]
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
			add(e.GitEmail[i+1:])
		}
	}
	add(e.RepoOwner)
	add(e.RepoName)
	return out
}
