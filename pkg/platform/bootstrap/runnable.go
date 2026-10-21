package bootstrap

import (
	"context"
	"fmt"
)

// SchemaWriter is the subset of spicedb.SchemaIOAdapter the bootstrap needs.
type SchemaWriter interface {
	WriteSchema(ctx context.Context, text string) error
}

// RunBootstrap is the operator's leader-only bootstrap step. It applies the
// SpiceDB base schema idempotently (WriteSchema is a full-schema replace).
//
// It deliberately does NOT mint Secrets: the internal Secrets are minted by the
// pre-start bootstrap Job (MintAllInternalSecrets), which runs BEFORE the
// operator so every consumer mounts its Secret cleanly at first boot — no
// mint-after-start, no pod rolls. Schema stays here because it is the one step
// that needs SpiceDB already running (the Job runs before SpiceDB is up).
//
// Safe to run on every leader acquisition: WriteSchema is idempotent.
func RunBootstrap(ctx context.Context, schemaIO SchemaWriter, schemaText string) error {
	if err := schemaIO.WriteSchema(ctx, schemaText); err != nil {
		return fmt.Errorf("apply spicedb schema: %w", err)
	}
	return nil
}
