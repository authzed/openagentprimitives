package sqlite

// ftsMigrateSQL creates the FTS5 virtual table. Key columns are
// UNINDEXED (stored, not tokenized) so we can scope + reconstruct the
// entry; only content is tokenized. 'porter unicode61' gives stemming
// parity with the postgres provider's to_tsvector('english', ...).
const ftsMigrateSQL = `
CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(
    scope_kind UNINDEXED,
    scope_id   UNINDEXED,
    kind       UNINDEXED,
    entry_id   UNINDEXED,
    content,
    tokenize = 'porter unicode61'
);
`
