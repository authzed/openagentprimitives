package oci

import (
	"context"
	"fmt"
	"os"

	"oras.land/oras-go/v2"
	ocilayout "oras.land/oras-go/v2/content/oci"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// Push loads the OCI-layout tar in oapBytes (pkg/platform/oap.Pack's output) as a
// read-only source store and copies its manifest graph (config + layers) to
// the remote repository ref. It returns the manifest digest — the same value
// oap.Digest(oapBytes) reports, since the copy is content-addressed end to
// end and verifies nothing changes in flight.
func Push(ctx context.Context, ref string, oapBytes []byte, opts Options) (string, error) {
	repo, err := repository(ref, opts)
	if err != nil {
		return "", err
	}

	localDigest, err := oap.Digest(oapBytes)
	if err != nil {
		return "", fmt.Errorf("push %s: %w", ref, err)
	}

	// content/oci's read-only store wants a tar *file* on disk — it indexes
	// entry offsets up front and seeks into the file lazily on Fetch. Our .oap
	// bytes are already exactly that OCI-layout tar, so hand it a temp file
	// rather than untarring by hand into a directory.
	tmp, err := os.CreateTemp("", "oap-push-*.tar")
	if err != nil {
		return "", fmt.Errorf("push %s: create temp tar: %w", ref, err)
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.Write(oapBytes)
	closeErr := tmp.Close()
	if writeErr != nil {
		return "", fmt.Errorf("push %s: write temp tar: %w", ref, writeErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("push %s: close temp tar: %w", ref, closeErr)
	}

	src, err := ocilayout.NewFromTar(ctx, tmp.Name())
	if err != nil {
		return "", fmt.Errorf("push %s: load .oap as oci-layout source: %w", ref, err)
	}

	// Every manifest in a .oap's index.json is tagged by its own digest
	// string (regardless of the AnnotationRefName Pack sets), so localDigest
	// always resolves against src.
	dstRef := repo.Reference.ReferenceOrDefault()
	desc, err := oras.Copy(ctx, src, localDigest, repo, dstRef, oras.DefaultCopyOptions)
	if err != nil {
		return "", fmt.Errorf("push %s: copy to registry: %w", ref, err)
	}
	return desc.Digest.String(), nil
}
