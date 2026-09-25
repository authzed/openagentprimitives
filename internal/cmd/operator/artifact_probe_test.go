package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
	blobstore "github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
)

// TestProbeArtifactStore_HappyPathSucceeds exercises the real Put/Get/Delete
// self-check against an in-process store; a healthy backend must pass on the
// first attempt with no error.
func TestProbeArtifactStore_HappyPathSucceeds(t *testing.T) {
	store := blobstore.NewMem()
	t.Cleanup(func() { _ = store.Close() })

	err := probeArtifactStore(context.Background(), logr.Discard(), store, "mem://")
	require.NoError(t, err)
}

// alwaysFailStore is a minimal artifactstore.Store fake whose Put always
// errors, so probeArtifactStore's first sub-step fails on every attempt. The
// unexercised methods are never reached by the probe; they return
// not-implemented errors rather than zero values so a future change to the
// probe's call order fails loudly instead of silently passing.
type alwaysFailStore struct{}

func (alwaysFailStore) Put(context.Context, string, io.Reader) (artifactstore.Ref, error) {
	return "", errors.New("simulated backend failure")
}

func (alwaysFailStore) Get(context.Context, artifactstore.Ref) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}

func (alwaysFailStore) Delete(context.Context, artifactstore.Ref) error {
	return errors.New("not implemented")
}

func (alwaysFailStore) Exists(context.Context, artifactstore.Ref) (bool, error) {
	return false, errors.New("not implemented")
}

func (alwaysFailStore) List(context.Context, artifactstore.ListOpts) ([]artifactstore.Item, string, error) {
	return nil, "", errors.New("not implemented")
}

func (alwaysFailStore) Key(artifactstore.Ref) (string, error) {
	return "", errors.New("not implemented")
}

// TestProbeArtifactStore_AlwaysFailingStoreReturnsBoundedError bounds the
// wait via a short ctx deadline rather than shortening artifactProbeCeiling:
// unlike openPostgresWithRetry, probeArtifactStore does not take ceiling as a
// parameter (it closes over the package-private 2-minute
// artifactProbeCeiling const), so a test cannot inject a tiny ceiling the way
// TestOpenPostgresWithRetry_CeilingExceededReturnsLoudError does. Instead,
// startup.Retry selects on ctx.Done() between backoff attempts, so a
// short-lived ctx deadline exits the retry loop long before the real
// 2-minute production ceiling ever would — the loop returns ctx.Err()
// (context.DeadlineExceeded) rather than the op-named "not ready after"
// wrap that the ceiling-exceeded path produces, since that path is not the
// one exercised here.
func TestProbeArtifactStore_AlwaysFailingStoreReturnsBoundedError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := probeArtifactStore(ctx, logr.Discard(), alwaysFailStore{}, "mem://")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}
