package share

import (
	"math"
	"net"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

// secretPattern's final alternative catches a dashed UUID (8-4-4-4-12 hex groups): a run or
// request ID naming one is not itself a secret token, but it always identifies one specific
// private run, so it is judged secret rather than let through as prose.
var secretPattern = regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{8,}|\bsk-[A-Za-z0-9_\-]{16,}|\bgsk_[A-Za-z0-9]{16,}|\bnvapi-[A-Za-z0-9_\-]{16,}|\bgh[pousr]_[A-Za-z0-9]{20,}|\bgithub_pat_[A-Za-z0-9_]{20,}|\bAIza[0-9A-Za-z_\-]{30,}|\bAKIA[0-9A-Z]{16}\b|\bxox[abp]-[A-Za-z0-9\-]{10,}|\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}|-----BEGIN [A-Z ]*PRIVATE KEY-----|(?i:\bbearer\s+[A-Za-z0-9._~+/\-]{16,})|\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)

// hexSecretPattern catches a long hex-only run (a hex-encoded key or a UUID used as one):
// Shannon entropy tops out at exactly 4.0 for a 16-symbol alphabet, so it can never exceed
// minTokenEntropy and needs its own check.
var hexSecretPattern = regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`)

// credentialPattern catches a plain "keyword = value" or "keyword: value" credential that has
// no distinctive shape of its own, such as password=Summer2024!. The leading boundary is
// "not a letter or digit" rather than \b, so an underscore still counts as a boundary (an
// env-var name like DB_PASSWORD or GITHUB_TOKEN triggers); an optional closing quote is
// allowed between the keyword and the separator, so a quoted JSON key like "password": ...
// also triggers. The keyword may carry any number of "_"/"-"-joined word suffixes
// (STRIPE_SECRET_KEY, DB_PASS_PROD), so a short or otherwise unremarkable value after it is
// still judged a credential; a bare-letter suffix with no separator is not accepted, so
// max_tokens: 4096 is not "token" plus a suffix. A lowercase letter immediately before a
// capitalized keyword (dbPassword, clientSecret, apiToken) is also a boundary, for a camelCase
// name that the non-alphanumeric boundary alone would miss. A suffix may also be joined with a
// dot (secret.key:) or glued on as a capitalized camelCase word (secretKey:, apiTokenProd:);
// the camelCase suffix is case-sensitive, so a lowercase continuation (tokens:) still does not
// count.
var credentialPattern = regexp.MustCompile(`(?:(?i:(?:^|[^A-Za-z0-9])(?:pass(?:word|wd)?|pwd|secret|api[_-]?key|access[_-]?token|auth[_-]?token|token))|[a-z](?:Pass(?:word|wd)?|Pwd|Secret|Api[_-]?Key|Access[_-]?Token|Auth[_-]?Token|Token))(?:[_.-][A-Za-z0-9]+|[A-Z][A-Za-z0-9]*)*["']?\s*[:=]\s*\S+`)

var emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// phonePattern uses [ \t] rather than \s so a match cannot span a newline.
var phonePattern = regexp.MustCompile(`\+?\(?\d[\d \t().\-]{7,}\d`)

var ipv4Pattern = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)

func hasSecret(q string) bool {
	if secretPattern.MatchString(q) || hexSecretPattern.MatchString(q) || credentialPattern.MatchString(q) || hasURLPassword(q) {
		return true
	}
	for _, tok := range tokens(q) {
		// URLs are judged by the url rule; a random-looking key mixes letters and digits.
		if len(tok) < minEntropyTokenLen || strings.Contains(tok, "://") || !hasLetterAndDigit(tok) {
			continue
		}
		if shannonEntropy(tok) > minTokenEntropy {
			return true
		}
	}
	return false
}

// hasURLPassword reports a URL whose userinfo carries a password, such as
// scheme://user:pass@host. That is a credential, not merely a link, so it is judged secret rather
// than url, whether or not the host is allowlisted.
func hasURLPassword(q string) bool {
	for _, raw := range urlPattern.FindAllString(q, -1) {
		u, err := url.Parse(strings.TrimRight(raw, ".,:;!?"))
		if err != nil || u.User == nil {
			continue
		}
		if _, ok := u.User.Password(); ok {
			return true
		}
	}
	return false
}

func hasLetterAndDigit(s string) bool {
	var letter, digit bool
	for _, r := range s {
		letter = letter || unicode.IsLetter(r)
		digit = digit || unicode.IsDigit(r)
	}
	return letter && digit
}

func shannonEntropy(s string) float64 {
	counts := map[rune]int{}
	n := 0
	for _, r := range s {
		counts[r]++
		n++
	}
	var h float64
	for _, c := range counts {
		p := float64(c) / float64(n)
		h -= p * math.Log2(p)
	}
	return h
}

func hasPII(q string) bool {
	if emailPattern.MatchString(q) {
		return true
	}
	for _, m := range phonePattern.FindAllString(q, -1) {
		digits := 0
		for _, r := range m {
			if r >= '0' && r <= '9' {
				digits++
			}
		}
		if digits >= minPhoneDigits {
			return true
		}
	}
	for _, m := range ipv4Pattern.FindAllString(q, -1) {
		if ip := net.ParseIP(m); ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
			return true
		}
	}
	for _, tok := range tokens(q) {
		if strings.Count(tok, ":") < 2 {
			continue
		}
		t := strings.TrimRight(strings.Trim(tok, "[]"), ".?!")
		// Cut at a zone id (%eth0) or a CIDR suffix (/48), whichever comes first, so
		// net.ParseIP sees only the address.
		if i := strings.IndexAny(t, "/%"); i >= 0 {
			t = t[:i]
		}
		if ip := net.ParseIP(t); ip != nil && ip.To4() == nil && !ip.IsLoopback() && !ip.IsUnspecified() {
			return true
		}
	}
	return false
}
