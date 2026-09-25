package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Migrate applies search-specific schema additions to memory_entry.
// The tsvector column (full-text search) is always added. The pgvector
// column (embedding similarity) is best-effort: if the vector extension
// is not installed, tsvector still works. Safe to call repeatedly.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, tsvectorMigrateSQL); err != nil {
		return fmt.Errorf("search postgres: tsvector migrate: %w", err)
	}
	if _, err := pool.Exec(ctx, pgvectorMigrateSQL); err != nil {
		// pgvector not installed is expected in dev; text search still works.
		return fmt.Errorf("search postgres: pgvector migrate (non-fatal): %w", err)
	}
	return nil
}

const tsvectorMigrateSQL = `
ALTER TABLE memory_entry ADD COLUMN IF NOT EXISTS content_tsv tsvector
    GENERATED ALWAYS AS (to_tsvector('english', COALESCE(content::text, ''))) STORED;
CREATE INDEX IF NOT EXISTS idx_entry_content_tsv ON memory_entry USING GIN (content_tsv);
`

const pgvectorMigrateSQL = `
CREATE EXTENSION IF NOT EXISTS vector;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'memory_entry' AND column_name = 'embedding'
    ) THEN
        EXECUTE format('ALTER TABLE memory_entry ADD COLUMN embedding vector(%s)',
            COALESCE(current_setting('app.embedding_dimension', true), '1024'));
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_entry_embedding_hnsw ON memory_entry
    USING hnsw (embedding vector_cosine_ops);
`
