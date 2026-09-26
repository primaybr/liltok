package db

import "context"

// PurgeCacheLargerThan deletes unpinned cache entries whose normalized prompt is longer than
// maxBytes, with their search rows, and reports how many entries it removed. Entries that large are
// almost always agent session transcripts that never repeat; they dominate the database size.
func (d *DB) PurgeCacheLargerThan(ctx context.Context, maxBytes int) (int64, error) {
	res, err := d.ExecContext(ctx, `DELETE FROM cache_entries WHERE is_pinned = 0 AND octet_length(normalized_prompt) > ?`, maxBytes)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		if err := d.PruneCacheSearch(ctx); err != nil {
			return n, err
		}
	}
	return n, nil
}
