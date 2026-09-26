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
	// Task 7-fix, item 5 added the git email's local part as its own term; "someone" (7 chars) is
	// not already present, unlike the "alice" case in TestAutoDenyTerms above, so it is now emitted
	// alongside the full address. "gmail.com" is still excluded as a generic mail domain.
	want := []string{"someone@gmail.com", "someone"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AutoDenyTerms = %q, want %q", got, want)
	}
}

// TestAutoDenyTermsSkipsGenericAccountName covers task 7-fix item 4: a username that is a generic
// OS account name plus an optional number ("Windows 11") identifies the machine, not the
// contributor, so it must not be emitted even after the domain-prefix strip.
func TestAutoDenyTermsSkipsGenericAccountName(t *testing.T) {
	got := share.AutoDenyTerms(share.DenyEnv{Username: `HOST\Windows 11`})
	if len(got) != 0 {
		t.Errorf("AutoDenyTerms = %q, want none", got)
	}
}

// TestAutoDenyTermsGitEmailLocalPart covers task 7-fix item 5: the git email's local part (the part
// before "@") is added as its own term when it is five or more characters, in addition to the full
// address; the public mail domain is still excluded.
func TestAutoDenyTermsGitEmailLocalPart(t *testing.T) {
	got := share.AutoDenyTerms(share.DenyEnv{GitEmail: "jdoe.builds@gmail.com"})
	want := []string{"jdoe.builds@gmail.com", "jdoe.builds"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AutoDenyTerms = %q, want %q", got, want)
	}
}

// TestAutoDenyTermsRepoIdentity covers task 7-fix item 5: the repository owner and name are emitted
// as deny terms subject to the same length and generic-name filters as everything else.
func TestAutoDenyTermsRepoIdentity(t *testing.T) {
	got := share.AutoDenyTerms(share.DenyEnv{RepoOwner: "jdoe-dev", RepoName: "widgetgate"})
	want := []string{"jdoe-dev", "widgetgate"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AutoDenyTerms = %q, want %q", got, want)
	}
}
