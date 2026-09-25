-- Model health learned from upstream replies: models that answered "not found" or "decommissioned"
-- are marked inactive and skipped in fallback chains until their recheck time.
CREATE TABLE IF NOT EXISTS model_catalog (
    provider TEXT NOT NULL,
    model TEXT NOT NULL COLLATE NOCASE,
    status TEXT NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    checked_at TEXT NOT NULL,
    fail_count INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (provider, model)
);
