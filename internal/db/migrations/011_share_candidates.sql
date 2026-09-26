-- Questions from the local cache that passed the share gate (liltok share scan) and wait for the
-- user's review. source_hash is the local cache entry and is never exported.
CREATE TABLE IF NOT EXISTS share_candidates (
    id TEXT PRIMARY KEY,
    question TEXT NOT NULL,
    source_hash TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected', 'exported')),
    gate_version INTEGER NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_share_candidates_status ON share_candidates(status);
