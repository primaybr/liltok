-- Persistent project memories and conventions discovered across agent sessions (Hermes Agent pattern).
CREATE TABLE IF NOT EXISTS gateway_memories (
    id TEXT PRIMARY KEY,
    project_key TEXT NOT NULL,
    category TEXT NOT NULL DEFAULT 'convention' CHECK (category IN ('convention', 'environment', 'quirk', 'architecture', 'user')),
    content TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_gateway_memories_project ON gateway_memories(project_key);
CREATE INDEX IF NOT EXISTS idx_gateway_memories_category ON gateway_memories(category);
