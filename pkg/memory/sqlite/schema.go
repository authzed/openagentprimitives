package sqlite

// migrateSQL mirrors the postgres memory schema in SQLite dialect. created_at is
// INTEGER unix-nanoseconds UTC — exact and unambiguously sortable, unlike a
// trimmed RFC3339Nano string. tags is a JSON array TEXT filtered with json_each.
// links is denormalized JSON on the entry for round-trip PLUS a memory_link
// table for traversal. provenance is the append-only tamper-evidence envelope as
// JSON TEXT, NULL on mutable kinds.
const migrateSQL = `
CREATE TABLE IF NOT EXISTS memory_entry (
    scope_kind TEXT NOT NULL,
    scope_id   TEXT NOT NULL,
    kind       TEXT NOT NULL,
    entry_id   TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    tags       TEXT NOT NULL DEFAULT '[]',
    content    TEXT,
    links      TEXT NOT NULL DEFAULT '[]',
    provenance TEXT,
    PRIMARY KEY (scope_kind, scope_id, kind, entry_id)
);
CREATE INDEX IF NOT EXISTS idx_entry_scope ON memory_entry (scope_kind, scope_id);
CREATE INDEX IF NOT EXISTS idx_entry_scope_kind ON memory_entry (scope_kind, scope_id, kind);
CREATE INDEX IF NOT EXISTS idx_entry_created ON memory_entry (scope_kind, scope_id, kind, created_at);

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

// addedColumns carries columns that must reach ALREADY-INSTALLED databases.
// CREATE TABLE IF NOT EXISTS is a no-op against an existing database, so a
// column added after the first shipped shape arrives only through an ALTER, and
// SQLite has no IF NOT EXISTS on ADD COLUMN — Client.Migrate probes for each
// column first.
//
// A row here is the whole cost of a new nullable column, but the declaration
// must be one SQLite ADD COLUMN accepts: no NOT NULL without a default, no
// PRIMARY KEY or UNIQUE.
var addedColumns = []struct{ table, column, decl string }{
	// Without this, a database created before the tamper-evidence envelope
	// existed would keep silently dropping it.
	{table: "memory_entry", column: "provenance", decl: "TEXT"},
}
