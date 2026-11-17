package bootstrap

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/wait"
)

// SchemaIO is the subset of spicedb.SchemaIOAdapter the bootstrap needs.
type SchemaIO interface {
	ReadSchema(ctx context.Context) (string, error)
	WriteSchema(ctx context.Context, text string) error
}

// SeedBackoff is the retry schedule RunBootstrap uses while SpiceDB is
// unreachable (a GitOps cold start brings the operator up before SpiceDB). It
// retries until the context ends; Steps only bounds how far the delay grows.
var SeedBackoff = wait.Backoff{
	Duration: 2 * time.Second,
	Factor:   2,
	Jitter:   0.1,
	Steps:    6,
	Cap:      time.Minute,
}

// RunBootstrap is the operator's leader-only bootstrap step: it seeds the
// SpiceDB base schema on a datastore that has NONE, and otherwise leaves the
// live schema alone.
//
// It must never overwrite a non-empty schema. WriteSchema is a full replace, and
// the live text is the guardian's composition — scaffold plus every MCPServer,
// SidecarToolbox, toolkit and bootstrap fragment. Replacing it with the bare
// scaffold drops those definitions, and SpiceDB refuses the write outright once
// relationships exist on them. Only the guardian reconciler writes a populated
// schema.
//
// It deliberately does NOT mint Secrets: the internal Secrets are minted by the
// pre-start bootstrap Job (MintAllInternalSecrets), which runs BEFORE the
// operator so every consumer mounts its Secret cleanly at first boot.
//
// A SpiceDB failure is retried with backoff and logged, never returned: a
// RunnableFunc error stops the whole manager, and an operator that crash-loops
// because SpiceDB is still starting is the failure this step must not cause.
// It returns only when the seed check has succeeded or ctx ends.
func RunBootstrap(ctx context.Context, logger logr.Logger, schemaIO SchemaIO, schemaText string) error {
	backoff := SeedBackoff
	for {
		seeded, err := seedSchemaIfEmpty(ctx, schemaIO, schemaText)
		if err == nil {
			if seeded {
				logger.Info("bootstrap: seeded the SpiceDB base schema on an empty datastore")
			} else {
				logger.Info("bootstrap: SpiceDB schema already present; leaving it to the guardian")
			}
			return nil
		}
		delay := backoff.Step()
		logger.Info("bootstrap: SpiceDB schema seed failed; retrying", "err", err.Error(), "retryIn", delay.String())
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

// seedSchemaIfEmpty writes schemaText only when the live schema has no
// definitions. SpiceDB answers ReadSchema on an empty datastore with NotFound.
func seedSchemaIfEmpty(ctx context.Context, schemaIO SchemaIO, schemaText string) (bool, error) {
	cur, err := schemaIO.ReadSchema(ctx)
	switch {
	case status.Code(err) == codes.NotFound:
	case err != nil:
		return false, fmt.Errorf("read spicedb schema: %w", err)
	case strings.Contains(cur, "definition "):
		return false, nil
	}
	if err := schemaIO.WriteSchema(ctx, schemaText); err != nil {
		return false, fmt.Errorf("write spicedb base schema: %w", err)
	}
	return true, nil
}
