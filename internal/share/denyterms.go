package share

import (
	"os"
	"os/exec"
	"os/user"
	"strings"
	"unicode/utf8"
)

// DenyEnv is the identity of the machine a scan runs on.
type DenyEnv struct {
	Username string
	Hostname string
	GitName  string
	GitEmail string
}

// genericNames are account and host names too common to identify anyone; using them as deny terms
// would reject ordinary questions ("run as root", "bind to localhost").
var genericNames = map[string]bool{
	"root": true, "admin": true, "administrator": true, "user": true, "dev": true, "developer": true,
	"ubuntu": true, "runner": true, "localhost": true, "home": true, "desktop": true, "laptop": true, "pc": true,
}

// genericDomains are public mail domains; an address there identifies its owner, its domain does not.
var genericDomains = map[string]bool{
	"gmail.com": true, "googlemail.com": true, "outlook.com": true, "hotmail.com": true, "live.com": true,
	"yahoo.com": true, "icloud.com": true, "me.com": true, "proton.me": true, "protonmail.com": true,
	"users.noreply.github.com": true,
}

// CurrentDenyEnv reads the OS username, hostname and git identity. Missing values stay empty.
func CurrentDenyEnv() DenyEnv {
	var e DenyEnv
	if u, err := user.Current(); err == nil {
		e.Username = u.Username
	}
	e.Hostname, _ = os.Hostname()
	e.GitName = gitConfig("user.name")
	e.GitEmail = gitConfig("user.email")
	return e
}

func gitConfig(key string) string {
	out, err := exec.Command("git", "config", "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// AutoDenyTerms returns the identity terms to reject questions on: the username without a domain
// prefix, the hostname and its first label, the git name, and the git email and its domain.
// Generic names and public mail domains are left out; terms shorter than three characters too.
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
	add(u)
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
			add(e.GitEmail[i+1:])
		}
	}
	return out
}
