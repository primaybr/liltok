package share

import (
	"net"
	"net/url"
	"regexp"
	"strings"
)

// urlPattern accepts any scheme, not just http(s)/ftp, so a scheme like postgres:// or redis://
// is judged instead of silently passing.
var urlPattern = regexp.MustCompile(`(?i)\b[A-Za-z][A-Za-z0-9+.\-]*://[^\s"'<>()\[\]{}]+`)

// bareHostPattern matches a scheme-less "host.tld/path" mention: one or more dotted labels
// ending in a letters-only label of two or more characters, an optional :port, then a slash
// and a path. A reader still resolves this as a link, so it is judged by the same allowlist
// as a full URL. The token is stripped of a leading protocol-relative "//" and leading or
// trailing markdown emphasis characters before this pattern is tried; see hasForeignURL.
var bareHostPattern = regexp.MustCompile(`^(?:[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?\.)+[A-Za-z]{2,}(?::\d+)?/\S*$`)

// privateHostSuffixPattern matches a scheme-less, path-less hostname whose final label is a
// private-network suffix (internal, corp, local, lan, intranet): a reader still recognizes this
// as a private host name even with no path to resolve. "arpa" (reverse-DNS infrastructure, as in
// in-addr.arpa) is deliberately excluded, since it names public resolver zones, not a private
// host.
var privateHostSuffixPattern = regexp.MustCompile(`(?i)^(?:[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?\.)+(?:internal|corp|local|lan|intranet)(?::\d+)?$`)

// hasForeignURL reports a URL whose host is not on the allowlist, or whose userinfo names a user
// with no password (a password is judged secret instead; see hasURLPassword, checked earlier). It
// also treats a scheme-less "host.tld/path" mention as a URL, since a reader still resolves it as
// a link, and a scheme-less hostname ending in a private-network suffix even with no path (see
// privateHostSuffixPattern). A loopback host (localhost, 127.0.0.1, or the IPv6 loopback) is exempt
// either way, the same way a bare loopback address is already exempt from the pii rule, and so is a
// fixed container host name (see containerHostNames).
func (g *Gate) hasForeignURL(q string) bool {
	for _, raw := range urlPattern.FindAllString(q, -1) {
		u, err := url.Parse(strings.TrimRight(raw, ".,:;!?"))
		if err != nil {
			return true
		}
		// Userinfo is judged before the loopback exemption, so http://admin@localhost/ still rejects.
		if u.User != nil {
			return true
		}
		host := strings.ToLower(u.Hostname())
		if isLoopbackHost(host) || containerHostNames[host] {
			continue
		}
		if !g.hostAllowed(host) {
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
		if isLoopbackHost(host) || containerHostNames[host] {
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

// containerHostNames are the host names Docker Desktop and Podman define identically on every
// install to reach the host machine or its gateway from inside a container. Like loopback, they
// name no one's network. Only these exact names are exempt: a subdomain of one, or a cluster
// service name such as "billing.payments.svc.cluster.local", still names a private host.
var containerHostNames = map[string]bool{
	"host.docker.internal":       true,
	"gateway.docker.internal":    true,
	"kubernetes.docker.internal": true,
	"host.containers.internal":   true,
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
