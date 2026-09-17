-- 1. Cache Entries Table (Tier-1 and Tier-3 Storage)
CREATE TABLE IF NOT EXISTS cache_entries (
    hash TEXT PRIMARY KEY,
    model TEXT NOT NULL,
    normalized_prompt TEXT NOT NULL,
    response_payload BLOB NOT NULL,
    prompt_tokens INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    hit_count INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_accessed_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    ttl_seconds INTEGER NOT NULL DEFAULT 604800,
    is_pinned BOOLEAN NOT NULL DEFAULT 0,
    is_semantic BOOLEAN NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_cache_entries_last_accessed 
ON cache_entries (last_accessed_at);

CREATE INDEX IF NOT EXISTS idx_cache_entries_model 
ON cache_entries (model);

-- 2. Cache Tags (For granular invalidation)
CREATE TABLE IF NOT EXISTS cache_tags (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    cache_hash TEXT NOT NULL,
    tag TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (cache_hash) REFERENCES cache_entries(hash) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_cache_tags_tag 
ON cache_tags (tag);

-- 3. Semantic Vector Storage (Tier-3 Semantic Index)
CREATE TABLE IF NOT EXISTS semantic_embeddings (
    cache_hash TEXT PRIMARY KEY,
    embedding BLOB NOT NULL,
    dimension INTEGER NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (cache_hash) REFERENCES cache_entries(hash) ON DELETE CASCADE
);

-- 4. Request Logs & Financial Ledger
CREATE TABLE IF NOT EXISTS request_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id TEXT NOT NULL UNIQUE,
    timestamp TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    api_key_id TEXT,
    model TEXT NOT NULL,
    provider TEXT NOT NULL,
    cache_status TEXT NOT NULL,
    cache_tier TEXT NOT NULL,
    prompt_tokens INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    cached_tokens INTEGER NOT NULL DEFAULT 0,
    latency_ms INTEGER NOT NULL DEFAULT 0,
    cost_usd REAL NOT NULL DEFAULT 0.0,
    saved_usd REAL NOT NULL DEFAULT 0.0,
    status_code INTEGER NOT NULL DEFAULT 200,
    error_message TEXT
);

CREATE INDEX IF NOT EXISTS idx_request_logs_timestamp 
ON request_logs (timestamp);

CREATE INDEX IF NOT EXISTS idx_request_logs_model 
ON request_logs (model);

CREATE INDEX IF NOT EXISTS idx_request_logs_cache_status 
ON request_logs (cache_status);

-- 5. Virtual API Keys
CREATE TABLE IF NOT EXISTS api_keys (
    id TEXT PRIMARY KEY,
    key_hash TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    rpm_limit INTEGER NOT NULL DEFAULT 60,
    tpm_limit INTEGER NOT NULL DEFAULT 100000,
    monthly_budget_usd REAL NOT NULL DEFAULT 0,
    current_spend_usd REAL NOT NULL DEFAULT 0,
    is_active BOOLEAN NOT NULL DEFAULT 1,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- 6. Model Pricing Matrix (Rates per 1 Million Tokens)
CREATE TABLE IF NOT EXISTS model_pricing (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    model_pattern TEXT NOT NULL UNIQUE,
    provider TEXT NOT NULL,
    tier TEXT NOT NULL DEFAULT 'premium',
    input_cost_per_m REAL NOT NULL,
    cached_input_cost_per_m REAL NOT NULL,
    output_cost_per_m REAL NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- 7. Provider Routing Chains & Circuit Targets
CREATE TABLE IF NOT EXISTS provider_routes (
    id TEXT PRIMARY KEY,
    strategy TEXT NOT NULL DEFAULT 'fallback',
    description TEXT,
    is_active BOOLEAN NOT NULL DEFAULT 1,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS route_targets (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    route_id TEXT NOT NULL,
    priority INTEGER NOT NULL DEFAULT 1,
    provider TEXT NOT NULL,
    upstream_model TEXT NOT NULL,
    timeout_ms INTEGER NOT NULL DEFAULT 60000,
    max_retries INTEGER NOT NULL DEFAULT 2,
    FOREIGN KEY (route_id) REFERENCES provider_routes(id) ON DELETE CASCADE
);

-- Seed Baseline Pricing
INSERT OR REPLACE INTO model_pricing (model_pattern, provider, tier, input_cost_per_m, cached_input_cost_per_m, output_cost_per_m)
VALUES 
    ('claude-3-5-sonnet.*', 'anthropic', 'premium', 3.00, 0.30, 15.00),
    ('claude-3-7-sonnet.*', 'anthropic', 'premium', 3.00, 0.30, 15.00),
    ('gpt-4o', 'openai', 'premium', 2.50, 1.25, 10.00),
    ('gpt-4o-mini', 'openai', 'budget', 0.15, 0.075, 0.60),
    ('deepseek-chat', 'deepseek', 'budget', 0.27, 0.07, 1.10),
    ('deepseek-reasoner', 'deepseek', 'budget', 0.55, 0.14, 2.19),
    ('meta/llama-3.3-70b-instruct', 'nvidianim', 'free', 0.00, 0.00, 0.00),
    ('deepseek-ai/deepseek-r1', 'nvidianim', 'free', 0.00, 0.00, 0.00),
    ('llama-3.3-70b-versatile', 'groq', 'free', 0.00, 0.00, 0.00),
    ('gemini-1.5-flash.*', 'gemini', 'free', 0.00, 0.00, 0.00),
    ('ollama/.*', 'ollama', 'free', 0.00, 0.00, 0.00);
