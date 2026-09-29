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

	res, err := d.ReconcileShareCandidates(ctx, []ShareCandidate{a, b}, 1, nil)
	if err != nil || res != (ShareReconcileResult{Added: 2}) {
		t.Fatalf("first scan = %+v, %v; want 2 added", res, err)
	}
	// Running the same scan again changes nothing.
	if res, err = d.ReconcileShareCandidates(ctx, []ShareCandidate{a, b}, 1, nil); err != nil || res != (ShareReconcileResult{}) {
		t.Fatalf("repeat scan = %+v, %v; want no changes", res, err)
	}

	// The user approves b and rejects c.
	if _, err := d.Exec(`UPDATE share_candidates SET status = 'approved' WHERE id = 'b'`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReconcileShareCandidates(ctx, []ShareCandidate{a, b, c}, 1, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE share_candidates SET status = 'rejected' WHERE id = 'c'`); err != nil {
		t.Fatal(err)
	}
	if res, err = d.ReconcileShareCandidates(ctx, []ShareCandidate{a, b, c}, 1, nil); err != nil || res != (ShareReconcileResult{}) {
		t.Fatalf("scan after decisions = %+v, %v; want no changes", res, err)
	}
	counts, err := d.CountShareCandidates(ctx)
	if err != nil || counts[ShareStatusPending] != 1 || counts[ShareStatusApproved] != 1 || counts[ShareStatusRejected] != 1 {
		t.Fatalf("counts = %v, %v; want 1 pending, 1 approved, 1 rejected", counts, err)
	}

	// Rules changed so that b and c no longer pass: approved b is dropped, rejected c is kept.
	if res, err = d.ReconcileShareCandidates(ctx, []ShareCandidate{a}, 1, nil); err != nil || res != (ShareReconcileResult{Dropped: 1}) {
		t.Fatalf("narrowed scan = %+v, %v; want 1 dropped", res, err)
	}
	// A gate-version bump requeues a surviving candidate.
	if res, err = d.ReconcileShareCandidates(ctx, []ShareCandidate{a}, 2, nil); err != nil || res != (ShareReconcileResult{Requeued: 1}) {
		t.Fatalf("version bump = %+v, %v; want 1 requeued", res, err)
	}
	counts, _ = d.CountShareCandidates(ctx)
	if counts[ShareStatusPending] != 1 || counts[ShareStatusRejected] != 1 || counts[ShareStatusApproved] != 0 {
		t.Errorf("final counts = %v", counts)
	}
}

// A pending or approved candidate the scan no longer returns is judged again by stillPasses: it is
// deleted only when it fails, so a question whose cache entry was purged keeps its approval.
func TestReconcileShareCandidatesKeepsPassingQuestionsWhoseSourceIsGone(t *testing.T) {
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")
	d, err := Open(filepath.Join(t.TempDir(), "share.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	mk := func(id string) ShareCandidate {
		return ShareCandidate{ID: id, Question: "question " + id, SourceHash: "h" + id}
	}
	a, b, c, dd, e := mk("a"), mk("b"), mk("c"), mk("d"), mk("e")
	if _, err := d.ReconcileShareCandidates(ctx, []ShareCandidate{a, b, c, dd, e}, 1, nil); err != nil {
		t.Fatal(err)
	}
	// b and d are approved, e is rejected; a and c stay pending.
	for id, status := range map[string]string{"b": "approved", "d": "approved", "e": "rejected"} {
		if _, err := d.Exec(`UPDATE share_candidates SET status = ? WHERE id = ?`, status, id); err != nil {
			t.Fatal(err)
		}
	}
	passes := map[string]bool{"question b": true}
	stillPasses := func(q string) bool { return passes[q] }

	// The scan returns only a. b still passes; c (pending) and d (approved) fail; e is rejected.
	res, err := d.ReconcileShareCandidates(ctx, []ShareCandidate{a}, 1, stillPasses)
	if err != nil || res != (ShareReconcileResult{Retained: 1, Dropped: 2}) {
		t.Fatalf("scan with sources gone = %+v, %v; want 1 retained, 2 dropped", res, err)
	}
	counts, _ := d.CountShareCandidates(ctx)
	if counts[ShareStatusPending] != 1 || counts[ShareStatusApproved] != 1 || counts[ShareStatusRejected] != 1 {
		t.Fatalf("counts = %v; want a pending, b approved, e rejected", counts)
	}
	// Repeating the scan reports the retained row again and changes nothing.
	if res, err = d.ReconcileShareCandidates(ctx, []ShareCandidate{a}, 1, stillPasses); err != nil || res != (ShareReconcileResult{Retained: 1}) {
		t.Fatalf("repeat scan = %+v, %v; want 1 retained", res, err)
	}

	// A gate-version bump sends a retained approval back to review, like any other open row.
	res, err = d.ReconcileShareCandidates(ctx, []ShareCandidate{a}, 2, stillPasses)
	if err != nil || res != (ShareReconcileResult{Requeued: 2}) {
		t.Fatalf("version bump = %+v, %v; want 2 requeued (a returned, b retained)", res, err)
	}
	counts, _ = d.CountShareCandidates(ctx)
	if counts[ShareStatusPending] != 2 || counts[ShareStatusApproved] != 0 || counts[ShareStatusRejected] != 1 {
		t.Fatalf("counts after bump = %v; want 2 pending, 1 rejected", counts)
	}

	// Without a judge the row is deleted, so a caller that forgets one never keeps an unchecked question.
	res, err = d.ReconcileShareCandidates(ctx, []ShareCandidate{a}, 2, nil)
	if err != nil || res != (ShareReconcileResult{Dropped: 1}) {
		t.Fatalf("nil judge = %+v, %v; want b dropped", res, err)
	}
}
