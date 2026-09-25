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

// TestResolve_MatchesPushedDigest stands up the same in-process fake registry
// TestPushPull_RoundTrip uses, Pushes a packed demo-agent .oap, then Resolves
// both the tag ref and the digest-pinned ref and asserts each returns exactly
// the digest Push reported — the invariant the `oap` pinning kind's Resolve
// depends on (it calls this function to freeze a tag ref to a manifest
// digest without pulling the artifact).
func TestResolve_MatchesPushedDigest(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")

	bundle, err := oap.FromFolder(oaptest.WriteBundle(t))
	require.NoError(t, err)
	packed, err := oap.Pack(bundle)
	require.NoError(t, err)

	tagRef := host + "/demo-agent:v1"
	opts := ociagent.Options{PlainHTTP: true}
	ctx := context.Background()

	pushedDigest, err := ociagent.Push(ctx, tagRef, packed, opts)
	require.NoError(t, err)

	gotDigest, err := ociagent.Resolve(ctx, tagRef, opts)
	require.NoError(t, err)
	assert.Equal(t, pushedDigest, gotDigest, "Resolve(tag ref) must match the digest Push reported")

	digestRef := host + "/demo-agent@" + pushedDigest
	gotFromDigestRef, err := ociagent.Resolve(ctx, digestRef, opts)
	require.NoError(t, err)
	assert.Equal(t, pushedDigest, gotFromDigestRef, "Resolve(digest ref) must return the same digest")
}

func TestResolve_EmbeddedDependencyDigest(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	bundle, err := oap.FromFolder(oaptest.WriteDependencyBundle(t))
	require.NoError(t, err)
	packed, err := oap.Pack(bundle)
	require.NoError(t, err)
	want, err := oap.Digest(packed)
	require.NoError(t, err)
	repo := strings.TrimPrefix(srv.URL, "http://") + "/embedded-agent"
	opts := ociagent.Options{PlainHTTP: true}
	pushed, err := ociagent.Push(context.Background(), repo+":v1", packed, opts)
	require.NoError(t, err)
	assert.Equal(t, want, pushed)
	for _, ref := range []string{repo + ":v1", repo + "@" + want} {
		got, err := ociagent.Resolve(context.Background(), ref, opts)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
}

// TestResolve_UnreachableHost exercises the error path — resolving against a
// host that refuses the connection must surface an error, not hang or panic.
func TestResolve_UnreachableHost(t *testing.T) {
	// Port 1 is privileged/unassigned; connection is refused immediately
	// rather than hanging.
	_, err := ociagent.Resolve(context.Background(), "127.0.0.1:1/does-not-exist:v1", ociagent.Options{PlainHTTP: true})
	assert.Error(t, err)
}
