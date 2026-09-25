// Package source builds an oap.Bundle from a place that holds an agent — a
// folder on disk today, a live cluster (cluster.go), a registry later.
package source

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// Source yields an in-memory oap.Bundle. Implementations must not read secret
// values — only references.
type Source interface {
	Bundle(ctx context.Context) (*oap.Bundle, error)
}
