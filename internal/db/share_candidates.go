package db

import "context"

// ShareCandidate is a question from the local cache that passed the share gate.
type ShareCandidate struct {
	ID          string
	Question    string
	SourceHash  string
	Status      string
	GateVersion int
}

// Share candidate statuses.
const (
	ShareStatusPending  = "pending"
	ShareStatusApproved = "approved"
	ShareStatusRejected = "rejected"
	ShareStatusExported = "exported"
)

// ShareReconcileResult counts what a scan changed.
type ShareReconcileResult struct {
	Added    int // new candidates
	Requeued int // pending or approved candidates gated again under a newer gate version
	Dropped  int // pending or approved candidates that no longer pass
}

// ReconcileShareCandidates makes share_candidates match the latest scan without undoing the user's
// decisions: new survivors are added as pending, pending and approved rows from an older gate
// version go back to pending, pending and approved rows that no longer pass are deleted, and
// rejected and exported rows are left alone.
func (d *DB) ReconcileShareCandidates(ctx context.Context, survivors []ShareCandidate, gateVersion int) (ShareReconcileResult, error) {
	var res ShareReconcileResult
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback() }()

	type row struct {
		status  string
		version int
	}
	existing := map[string]row{}
	rows, err := tx.QueryContext(ctx, `SELECT id, status, gate_version FROM share_candidates`)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var id string
		var r row
		if err := rows.Scan(&id, &r.status, &r.version); err != nil {
			rows.Close()
			return res, err
		}
		existing[id] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}

	open := func(status string) bool { return status == ShareStatusPending || status == ShareStatusApproved }
	keep := make(map[string]bool, len(survivors))
	for _, c := range survivors {
		keep[c.ID] = true
		old, ok := existing[c.ID]
		switch {
		case !ok:
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO share_candidates (id, question, source_hash, status, gate_version)
				VALUES (?, ?, ?, 'pending', ?)`, c.ID, c.Question, c.SourceHash, gateVersion); err != nil {
				return res, err
			}
			res.Added++
		case open(old.status) && old.version < gateVersion:
			if _, err := tx.ExecContext(ctx, `
				UPDATE share_candidates SET question = ?, source_hash = ?, status = 'pending', gate_version = ?,
					updated_at = CURRENT_TIMESTAMP
				WHERE id = ?`, c.Question, c.SourceHash, gateVersion, c.ID); err != nil {
				return res, err
			}
			res.Requeued++
		}
	}
	for id, r := range existing {
		if keep[id] || !open(r.status) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM share_candidates WHERE id = ?`, id); err != nil {
			return res, err
		}
		res.Dropped++
	}
	return res, tx.Commit()
}

// CountShareCandidates returns the number of candidates per status.
func (d *DB) CountShareCandidates(ctx context.Context) (map[string]int, error) {
	rows, err := d.QueryContext(ctx, `SELECT status, COUNT(*) FROM share_candidates GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}
