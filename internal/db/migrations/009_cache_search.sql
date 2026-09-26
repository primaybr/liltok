-- Short searchable summary per cache entry (latest and first user message, system reminders
-- removed). Searching normalized_prompt directly scans every full request, including multi-megabyte
-- agent transcripts; this table stays small. It is filled in the background by the gateway's search
-- indexer (internal/db/search.go), so entries written by any path become searchable.
CREATE TABLE IF NOT EXISTS cache_search (
    hash TEXT PRIMARY KEY,
    text TEXT NOT NULL
);
