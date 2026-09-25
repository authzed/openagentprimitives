package postgres

const migrateSQL = `
CREATE TABLE IF NOT EXISTS memory_entry (
    scope_kind  TEXT NOT NULL,
    scope_id    TEXT NOT NULL,
    kind        TEXT NOT NULL,
    entry_id    TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    tags        TEXT[] NOT NULL DEFAULT '{}',
    content     JSONB,
    links       JSONB NOT NULL DEFAULT '[]',
    provenance  JSONB,
    PRIMARY KEY (scope_kind, scope_id, kind, entry_id)
);
CREATE INDEX IF NOT EXISTS idx_entry_scope ON memory_entry (scope_kind, scope_id);
CREATE INDEX IF NOT EXISTS idx_entry_scope_kind ON memory_entry (scope_kind, scope_id, kind);
CREATE INDEX IF NOT EXISTS idx_entry_created ON memory_entry (scope_kind, scope_id, kind, created_at);
CREATE INDEX IF NOT EXISTS idx_entry_kind_created ON memory_entry (scope_kind, kind, created_at);
CREATE INDEX IF NOT EXISTS idx_entry_tags ON memory_entry USING GIN (tags);
ALTER TABLE memory_entry ADD COLUMN IF NOT EXISTS provenance JSONB;

CREATE TABLE IF NOT EXISTS memory_link (
    source_scope_kind TEXT NOT NULL,
    source_scope_id   TEXT NOT NULL,
    source_kind       TEXT NOT NULL,
    source_id         TEXT NOT NULL,
    relation          TEXT NOT NULL,
    target_scope_kind TEXT NOT NULL,
    target_scope_id   TEXT NOT NULL,
    target_kind       TEXT NOT NULL,
    target_id         TEXT NOT NULL,
    UNIQUE (source_scope_kind, source_scope_id, source_kind, source_id, relation, target_kind, target_id)
);
CREATE INDEX IF NOT EXISTS idx_link_source ON memory_link (source_scope_kind, source_scope_id, source_kind, source_id);
CREATE INDEX IF NOT EXISTS idx_link_target ON memory_link (target_scope_kind, target_scope_id, target_kind, target_id);
`
