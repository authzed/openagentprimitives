package oci

import (
	"context"
	"fmt"
)

// Resolve HEAD-resolves ref against the remote registry and returns its
// manifest digest ("sha256:…") WITHOUT pulling any blob content — the
// lightweight counterpart to Pull for callers (e.g. the `oap` pinning kind)
// that only need to learn what digest a ref currently points to, not fetch
// the artifact itself.
func Resolve(ctx context.Context, ref string, opts Options) (string, error) {
	repo, err := repository(ref, opts)
	if err != nil {
		return "", err
	}
	desc, err := repo.Resolve(ctx, repo.Reference.ReferenceOrDefault())
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", ref, err)
	}
	return desc.Digest.String(), nil
}
