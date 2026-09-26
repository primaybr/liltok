package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/primaybr/liltok/internal/telemetry"
)

// Cache search index. cache_entries.normalized_prompt holds the whole canonical request, including
// agent session transcripts of several megabytes, so matching text against it scans gigabytes. The
// cache_search table (migration 009) keeps a short, searchable summary per entry instead: the
// latest user message plus the first one, without client-injected <system-reminder> blocks. A
// background indexer fills rows for entries that lack one, whatever wrote the entry.

const (
	searchTextMax      = 1500 // bytes of the latest user message kept
	searchTaskMax      = 400  // bytes of the first user message kept, when it differs
	searchIndexBatch   = 25
	searchMaxWords     = 8
	searchCandidateCap = 200
)

var systemReminderBlock = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

// ExtractSearchText returns the searchable summary of a canonical request: the text of the latest
// user message that has any once system reminders are removed, followed by the first user message
// when it differs. A prompt that is not a JSON request with messages is used as plain text.
func ExtractSearchText(normalized string) string {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(normalized), &req); err != nil || len(req.Messages) == 0 {
		return clip(strings.TrimSpace(systemReminderBlock.ReplaceAllString(normalized, " ")), searchTextMax)
	}
	var first, last string
	for _, m := range req.Messages {
		if m.Role != "user" {
			continue
		}
		text := userText(m.Content)
		if text == "" {
			continue
		}
		if first == "" {
			first = text
		}
		last = text
	}
	out := clip(last, searchTextMax)
	if first != "" && first != last {
		out += "\n" + clip(first, searchTaskMax)
	}
	return out
}

// userText returns a message's own text: a string content, or its text blocks (tool results and
// images are skipped), with system reminders removed.
func userText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(systemReminderBlock.ReplaceAllString(s, " "))
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type != "text" {
			continue
		}
		if t := strings.TrimSpace(systemReminderBlock.ReplaceAllString(b.Text, " ")); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// IndexCacheSearch adds search rows for up to batch entries that have none and reports how many it
// added. It reads each entry's prompt individually so a batch of large transcripts stays bounded.
func (d *DB) IndexCacheSearch(ctx context.Context, batch int) (int, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT e.hash FROM cache_entries e
		WHERE NOT EXISTS (SELECT 1 FROM cache_search s WHERE s.hash = e.hash)
		LIMIT ?`, batch)
	if err != nil {
		return 0, err
	}
	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return 0, err
		}
		hashes = append(hashes, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	indexed := 0
	for _, h := range hashes {
		var prompt string
		if err := d.QueryRowContext(ctx, `SELECT normalized_prompt FROM cache_entries WHERE hash = ?`, h).Scan(&prompt); err != nil {
			if err == sql.ErrNoRows {
				continue
			}
			return indexed, err
		}
		if _, err := d.ExecContext(ctx, `INSERT OR REPLACE INTO cache_search (hash, text) VALUES (?, ?)`, h, ExtractSearchText(prompt)); err != nil {
			return indexed, err
		}
		indexed++
	}
	return indexed, nil
}

// PruneCacheSearch removes search rows whose cache entry is gone.
func (d *DB) PruneCacheSearch(ctx context.Context) error {
	_, err := d.ExecContext(ctx, `DELETE FROM cache_search WHERE hash NOT IN (SELECT hash FROM cache_entries)`)
	return err
}

// RunSearchIndexer keeps cache_search current until ctx ends: it indexes unindexed entries in small
// batches, pausing between batches so gateway traffic sharing the database is not held up, then
// prunes rows for deleted entries and waits interval before looking again.
func (d *DB) RunSearchIndexer(ctx context.Context, interval time.Duration) {
	for {
		total, start := 0, time.Now()
		for {
			n, err := d.IndexCacheSearch(ctx, searchIndexBatch)
			if err != nil {
				if ctx.Err() == nil {
					telemetry.Log.Warn().Err(err).Msg("Cache search indexing failed; retrying later")
				}
				break
			}
			total += n
			if n < searchIndexBatch {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
		if total > 0 {
			telemetry.Log.Info().Int("entries", total).Dur("took", time.Since(start)).Msg("Cache search index updated")
		}
		if err := d.PruneCacheSearch(ctx); err != nil && ctx.Err() == nil {
			telemetry.Log.Warn().Err(err).Msg("Cache search prune failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// SearchCacheHashes returns the hashes of entries whose search text contains every word of query
// (case-insensitive for ASCII), at most limit of them, most-hit first. Words shorter than two
// characters are ignored; at most searchMaxWords are used.
func (d *DB) SearchCacheHashes(ctx context.Context, query string, limit int) ([]string, error) {
	var words []string
	for _, w := range strings.Fields(strings.ToLower(query)) {
		if len(w) >= 2 {
			words = append(words, w)
		}
		if len(words) == searchMaxWords {
			break
		}
	}
	if len(words) == 0 || limit <= 0 {
		return nil, nil
	}
	clauses := make([]string, len(words))
	args := make([]interface{}, 0, len(words)+1)
	for i, w := range words {
		clauses[i] = `text LIKE ? ESCAPE '\'`
		args = append(args, "%"+escapeLike(w)+"%")
	}
	// Match on the small table first, then rank only the candidates by hits, so a common word never
	// makes the query read every large cache row.
	args = append(args, searchCandidateCap, limit)
	rows, err := d.QueryContext(ctx, `
		SELECT e.hash FROM cache_entries e
		WHERE e.hash IN (SELECT hash FROM cache_search WHERE `+strings.Join(clauses, " AND ")+` LIMIT ?)
		ORDER BY e.hit_count DESC, e.last_accessed_at DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// SearchTexts returns the stored search text for the given hashes (used as a prompt preview).
func (d *DB) SearchTexts(ctx context.Context, hashes []string) (map[string]string, error) {
	out := make(map[string]string, len(hashes))
	if len(hashes) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(hashes)), ",")
	args := make([]interface{}, len(hashes))
	for i, h := range hashes {
		args[i] = h
	}
	rows, err := d.QueryContext(ctx, `SELECT hash, text FROM cache_search WHERE hash IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var h, t string
		if err := rows.Scan(&h, &t); err != nil {
			return nil, err
		}
		out[h] = t
	}
	return out, rows.Err()
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
