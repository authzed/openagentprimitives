//go:build integration

package oci_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	ociagent "github.com/authzed/openagentprimitives/pkg/platform/oap/oci"
	"github.com/authzed/openagentprimitives/test/oaptest"
)

// TestPushPull_RoundTrip stands up a real, spec-compliant in-process
// registry (github.com/google/go-containerregistry/pkg/registry — an
// http.Handler purpose-built for exactly this: exercising registry clients in
// tests without a container), Pushes a packed demo-agent .oap to it over
// PlainHTTP, Pulls it back, and asserts the round-trip invariant: digests
// match end to end and the pulled bytes Unpack to the same Bundle, even
// though the pulled tar is not byte-identical to what was pushed (the outer
// container tar isn't itself content-addressed — only the OCI manifest/blobs
// inside it are).
func TestPushPull_RoundTrip(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")

	bundle, err := oap.FromFolder(oaptest.WriteBundle(t))
	require.NoError(t, err)
	packed, err := oap.Pack(bundle)
	require.NoError(t, err)
	localDigest, err := oap.Digest(packed)
	require.NoError(t, err)

	ref := host + "/demo-agent:v1"
	opts := ociagent.Options{PlainHTTP: true}
	ctx := context.Background()

	pushedDigest, err := ociagent.Push(ctx, ref, packed, opts)
	require.NoError(t, err)
	assert.Equal(t, localDigest, pushedDigest, "Push must return the same digest oap.Digest computes locally")

	pulled, pulledDigest, err := ociagent.Pull(ctx, ref, opts)
	require.NoError(t, err)
	assert.Equal(t, localDigest, pulledDigest, "Pull must return the digest that was pushed")

	pulledRecomputed, err := oap.Digest(pulled)
	require.NoError(t, err)
	assert.Equal(t, localDigest, pulledRecomputed, "oap.Digest(pulled) must match the local digest")

	wantBundle, err := oap.Unpack(packed)
	require.NoError(t, err)
	gotBundle, err := oap.Unpack(pulled)
	require.NoError(t, err)
	assert.Equal(t, wantBundle, gotBundle, "oap.Unpack(pulled) must deep-equal oap.Unpack(local)")
}

func TestPushPull_EmbeddedDependencies(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	bundle, err := oap.FromFolder(oaptest.WriteDependencyBundle(t))
	require.NoError(t, err)
	packed, err := oap.Pack(bundle)
	require.NoError(t, err)
	want, err := oap.Unpack(packed)
	require.NoError(t, err)
	require.Len(t, want.Dependencies, 1)
	localDigest, err := oap.Digest(packed)
	require.NoError(t, err)

	ref := strings.TrimPrefix(srv.URL, "http://") + "/embedded-agent:v1"
	opts := ociagent.Options{PlainHTTP: true}
	pushedDigest, err := ociagent.Push(context.Background(), ref, packed, opts)
	require.NoError(t, err)
	pulled, pulledDigest, err := ociagent.Pull(context.Background(), ref, opts)
	require.NoError(t, err)
	got, err := oap.Unpack(pulled)
	require.NoError(t, err)
	assert.Equal(t, localDigest, pushedDigest)
	assert.Equal(t, localDigest, pulledDigest)
	assert.Equal(t, want, got)
	require.Len(t, got.Dependencies, 1)
	assert.Equal(t, want.Dependencies[0].Packed, got.Dependencies[0].Packed, "OCI transport preserves nested archive bytes verbatim")
	require.NoError(t, oap.ValidateDependencyGraph(got))
}

// TestPush_UnreachableHost exercises the error path — a ref whose registry
// refuses the connection should surface as an error, not hang or panic.
func TestPush_UnreachableHost(t *testing.T) {
	bundle, err := oap.FromFolder(oaptest.WriteBundle(t))
	require.NoError(t, err)
	packed, err := oap.Pack(bundle)
	require.NoError(t, err)

	// Port 1 is privileged/unassigned; connection is refused immediately
	// rather than hanging.
	_, err = ociagent.Push(context.Background(), "127.0.0.1:1/does-not-exist:v1", packed, ociagent.Options{PlainHTTP: true})
	assert.Error(t, err)
}
