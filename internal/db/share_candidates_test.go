package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestReconcileShareCandidates(t *testing.T) {
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
	d, err := Open(filepath.Join(t.TempDir(), "share.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	a := ShareCandidate{ID: "a", Question: "question a", SourceHash: "ha"}
	b := ShareCandidate{ID: "b", Question: "question b", SourceHash: "hb"}
	c := ShareCandidate{ID: "c", Question: "question c", SourceHash: "hc"}

	res, err := d.ReconcileShareCandidates(ctx, []ShareCandidate{a, b}, 1)
	if err != nil || res != (ShareReconcileResult{Added: 2}) {
		t.Fatalf("first scan = %+v, %v; want 2 added", res, err)
	}
	// Running the same scan again changes nothing.
	if res, err = d.ReconcileShareCandidates(ctx, []ShareCandidate{a, b}, 1); err != nil || res != (ShareReconcileResult{}) {
		t.Fatalf("repeat scan = %+v, %v; want no changes", res, err)
	}

	// The user approves b and rejects c.
	if _, err := d.Exec(`UPDATE share_candidates SET status = 'approved' WHERE id = 'b'`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReconcileShareCandidates(ctx, []ShareCandidate{a, b, c}, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE share_candidates SET status = 'rejected' WHERE id = 'c'`); err != nil {
		t.Fatal(err)
	}
	if res, err = d.ReconcileShareCandidates(ctx, []ShareCandidate{a, b, c}, 1); err != nil || res != (ShareReconcileResult{}) {
		t.Fatalf("scan after decisions = %+v, %v; want no changes", res, err)
	}
	counts, err := d.CountShareCandidates(ctx)
	if err != nil || counts[ShareStatusPending] != 1 || counts[ShareStatusApproved] != 1 || counts[ShareStatusRejected] != 1 {
		t.Fatalf("counts = %v, %v; want 1 pending, 1 approved, 1 rejected", counts, err)
	}

	// Rules changed so that b and c no longer pass: approved b is dropped, rejected c is kept.
	if res, err = d.ReconcileShareCandidates(ctx, []ShareCandidate{a}, 1); err != nil || res != (ShareReconcileResult{Dropped: 1}) {
		t.Fatalf("narrowed scan = %+v, %v; want 1 dropped", res, err)
	}
	// A gate-version bump requeues a surviving candidate.
	if res, err = d.ReconcileShareCandidates(ctx, []ShareCandidate{a}, 2); err != nil || res != (ShareReconcileResult{Requeued: 1}) {
		t.Fatalf("version bump = %+v, %v; want 1 requeued", res, err)
	}
	counts, _ = d.CountShareCandidates(ctx)
	if counts[ShareStatusPending] != 1 || counts[ShareStatusRejected] != 1 || counts[ShareStatusApproved] != 0 {
		t.Errorf("final counts = %v", counts)
	}
}
