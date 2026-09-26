package share

import "testing"

// TestParseRepoURL covers task 7-fix item 5's pure URL-parsing helper directly, including the
// shapes CurrentDenyEnv feeds it from `git config --get remote.origin.url`.
func TestParseRepoURL(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		owner, repo string
	}{
		{"https with .git suffix", "https://git.example/jdoe-dev/widgetgate.git", "jdoe-dev", "widgetgate"},
		{"https without .git suffix", "https://git.example/jdoe-dev/widgetgate", "jdoe-dev", "widgetgate"},
		{"scp-like ssh form", "git@git.example:jdoe-dev/widgetgate.git", "jdoe-dev", "widgetgate"},
		{"trailing slash", "https://git.example/jdoe-dev/widgetgate/", "jdoe-dev", "widgetgate"},
		{"garbage input", "not a url at all", "", ""},
		{"empty input", "", "", ""},
		{"too few path segments", "https://git.example/widgetgate", "", ""},
		{"too many path segments", "https://git.example/group/jdoe-dev/widgetgate", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo := parseRepoURL(tc.raw)
			if owner != tc.owner || repo != tc.repo {
				t.Errorf("parseRepoURL(%q) = (%q, %q), want (%q, %q)", tc.raw, owner, repo, tc.owner, tc.repo)
			}
		})
	}
}
