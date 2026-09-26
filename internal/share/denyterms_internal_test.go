package share

import (
	"reflect"
	"testing"
)

// TestParseRepoURL covers parseRepoURL, the pure URL-parsing helper, directly, including the shapes
// CurrentDenyEnv feeds it from `git config --get remote.origin.url`.
func TestParseRepoURL(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		owner, repo string
		extra       []string
	}{
		{"https with .git suffix", "https://git.example/jdoe-dev/widgetgate.git", "jdoe-dev", "widgetgate", nil},
		{"https without .git suffix", "https://git.example/jdoe-dev/widgetgate", "jdoe-dev", "widgetgate", nil},
		{"scp-like ssh form", "git@git.example:jdoe-dev/widgetgate.git", "jdoe-dev", "widgetgate", nil},
		{"trailing slash", "https://git.example/jdoe-dev/widgetgate/", "jdoe-dev", "widgetgate", nil},
		{"garbage input", "not a url at all", "", "", nil},
		{"empty input", "", "", "", nil},
		{"too few path segments", "https://git.example/widgetgate", "", "", nil},
		// A GitLab-style subgroup remote has more than two path segments: the first segment is
		// still the owner and the last is still the repo name, and every segment between them is
		// also emitted, so a subgroup name is not silently dropped as a deny term.
		{"gitlab subgroup", "https://git.example/group/jdoe-dev/widgetgate", "group", "widgetgate", []string{"jdoe-dev"}},
		{"gitlab nested subgroups", "https://git.example/group/team/sub/widgetgate", "group", "widgetgate", []string{"team", "sub"}},
		// The SCP-like form is accepted for any "user@host:" prefix, not only "git@", and an
		// optional leading "/" after the colon is tolerated the same way a trailing ".git" is.
		{"scp-like form with a non-git user", "org-123@git.example:jdoe-dev/widgetgate.git", "jdoe-dev", "widgetgate", nil},
		{"scp-like form with a leading slash after the colon", "org-123@git.example:/jdoe-dev/widgetgate.git", "jdoe-dev", "widgetgate", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo, extra := parseRepoURL(tc.raw)
			if owner != tc.owner || repo != tc.repo || !reflect.DeepEqual(extra, tc.extra) {
				t.Errorf("parseRepoURL(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.raw, owner, repo, extra, tc.owner, tc.repo, tc.extra)
			}
		})
	}
}
