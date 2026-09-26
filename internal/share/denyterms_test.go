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
	want := []string{"someone@gmail.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AutoDenyTerms = %q, want %q", got, want)
	}
}
