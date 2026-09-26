package middleware

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// LocalGuard protects a gateway that trusts every caller (the admin API has no authentication and
// provider keys are applied server-side) from requests a web page in the user's browser can make:
//
//   - DNS rebinding: when the gateway listens on a loopback address, the Host header must be a
//     loopback name. A page that points its own domain at 127.0.0.1 sends its domain as Host and
//     is refused, so it cannot read or drive the gateway as if it were same-origin.
//   - Cross-site writes: a request with a method other than GET, HEAD or OPTIONS is refused when
//     the browser marks it as coming from another site (Sec-Fetch-Site, or an Origin that is not
//     this host). API clients, SDKs, curl and the CLI send neither header and are unaffected.
func LocalGuard(bindHost string) func(http.Handler) http.Handler {
	requireLoopbackHost := isLoopbackName(bindHost)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if requireLoopbackHost && !isLoopbackName(hostOnly(r.Host)) {
				refuse(w, "Host header must name this machine (localhost or a loopback address)")
				return
			}
			if !isSafeMethod(r.Method) && isCrossSite(r) {
				refuse(w, "cross-site requests are not accepted")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func isSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func isCrossSite(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "cross-site", "same-site":
		return true
	case "same-origin", "none":
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return true // includes the opaque "null" origin sent by sandboxed pages and file:// URLs
	}
	return !strings.EqualFold(u.Host, r.Host)
}

// hostOnly strips the port from a Host header value, including bracketed IPv6 ("[::1]:8080").
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

func isLoopbackName(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func refuse(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(ErrorResponse{Error: ErrorDetail{Message: msg, Type: "forbidden", Code: "request_not_allowed"}})
}
