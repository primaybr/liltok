package share_test

import (
	"reflect"
	"testing"

	"github.com/primaybr/liltok/internal/share"
)

func TestAutoDenyTerms(t *testing.T) {
	got := share.AutoDenyTerms(share.DenyEnv{
		Username: `CORP\alice`,
		Hostname: "build-box.corp.example",
		GitName:  "Alice Example",
		GitEmail: "alice@corp.example",
	})
	want := []string{"alice", "build-box.corp.example", "build-box", "Alice Example", "alice@corp.example", "corp.example"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AutoDenyTerms = %q, want %q", got, want)
	}
}

func TestAutoDenyTermsSkipsGenericNames(t *testing.T) {
	got := share.AutoDenyTerms(share.DenyEnv{Username: "root", Hostname: "localhost", GitName: "", GitEmail: "someone@gmail.com"})
	// The git email's local part is added as its own term; "someone" (7 chars) is not already
	// present, unlike the "alice" case in TestAutoDenyTerms above, so it is emitted alongside the
	// full address. "gmail.com" is still excluded as a generic mail domain.
	want := []string{"someone@gmail.com", "someone"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AutoDenyTerms = %q, want %q", got, want)
	}
}

// TestAutoDenyTermsSkipsGenericAccountName covers a username that is a generic OS account name plus
// an optional number ("Windows 11"): that identifies the machine, not the contributor, so it must
// not be emitted even after the domain-prefix strip.
func TestAutoDenyTermsSkipsGenericAccountName(t *testing.T) {
	got := share.AutoDenyTerms(share.DenyEnv{Username: `HOST\Windows 11`})
	if len(got) != 0 {
		t.Errorf("AutoDenyTerms = %q, want none", got)
	}
}

// TestAutoDenyTermsGitEmailLocalPart covers the git email's local part (the part before "@"): it is
// added as its own term when it is five or more characters, in addition to the full address; the
// public mail domain is still excluded.
func TestAutoDenyTermsGitEmailLocalPart(t *testing.T) {
	got := share.AutoDenyTerms(share.DenyEnv{GitEmail: "jdoe.builds@gmail.com"})
	want := []string{"jdoe.builds@gmail.com", "jdoe.builds"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AutoDenyTerms = %q, want %q", got, want)
	}
}

// TestAutoDenyTermsRepoIdentity covers the repository owner and name: they are emitted as deny terms
// subject to the same length and generic-name filters as everything else.
func TestAutoDenyTermsRepoIdentity(t *testing.T) {
	got := share.AutoDenyTerms(share.DenyEnv{RepoOwner: "jdoe-dev", RepoName: "widgetgate"})
	want := []string{"jdoe-dev", "widgetgate"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AutoDenyTerms = %q, want %q", got, want)
	}
}

// TestAutoDenyTermsRepoIdentitySubgroup covers a GitLab-style subgroup remote: parseRepoURL emits
// the segments between the owner and the repo name too, in path order between them, subject to the
// same filters as every other auto term.
func TestAutoDenyTermsRepoIdentitySubgroup(t *testing.T) {
	got := share.AutoDenyTerms(share.DenyEnv{RepoOwner: "group", RepoName: "widgetgate", RepoExtra: []string{"jdoe-dev"}})
	want := []string{"group", "jdoe-dev", "widgetgate"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AutoDenyTerms = %q, want %q", got, want)
	}
}

// TestAutoDenyTermsGitEmailPlusAddress covers a GitHub noreply address such as
// "12345+jdoe.dev@users.noreply.github.com": the numeric id dominates the local part's length check
// and the domain is generic, so the part before the first "+" and the part after the last "+" are
// also added as candidates.
func TestAutoDenyTermsGitEmailPlusAddress(t *testing.T) {
	got := share.AutoDenyTerms(share.DenyEnv{GitEmail: "12345+jdoe.dev@users.noreply.github.com"})
	want := []string{"12345+jdoe.dev@users.noreply.github.com", "12345+jdoe.dev", "12345", "jdoe.dev"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AutoDenyTerms = %q, want %q", got, want)
	}
}

// TestAutoDenyTermsGitEmailPlusAddressShortSuffix covers the "+"-split rule with a short after-"+"
// part: "ci" (2 chars) stays filtered out by the existing length check, while the before-"+" part
// "alice.w" (7 chars) is emitted.
func TestAutoDenyTermsGitEmailPlusAddressShortSuffix(t *testing.T) {
	got := share.AutoDenyTerms(share.DenyEnv{GitEmail: "alice.w+ci@corp.example"})
	want := []string{"alice.w+ci@corp.example", "alice.w+ci", "alice.w", "corp.example"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AutoDenyTerms = %q, want %q", got, want)
	}
}
