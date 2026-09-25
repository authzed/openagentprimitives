//go:build integration

package oap

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/pinning"
	oapbundle "github.com/authzed/openagentprimitives/pkg/platform/oap"
	ociagent "github.com/authzed/openagentprimitives/pkg/platform/oap/oci"
	"github.com/authzed/openagentprimitives/test/oaptest"
)

// withPlainHTTPFakeRegistry points package-private resolveOpts at PlainHTTP
// for the duration of the calling test, restoring the previous value on
// cleanup. Mirrors pkg/platform/identity/refresh's SetHTTPClient test-seam pattern —
// tests using it must not run t.Parallel() with each other (package-global
// state), which none of this package's tests do.
func withPlainHTTPFakeRegistry(t *testing.T) {
	t.Helper()
	prev := resolveOpts
	resolveOpts = ociagent.Options{PlainHTTP: true}
	t.Cleanup(func() { resolveOpts = prev })
}

// TestResolve_LiveDigestFromRegistry stands up the same in-process
// go-containerregistry fake registry pkg/platform/oap/oci's own round-trip tests use,
// pushes a packed demo-agent .oap under a tag ref, and drives the oap pinning
// Kind end to end: ParseRef classifies the tag as StrengthNamed, Resolve
// HEAD-resolves it to the digest Push reported (recording the tag as
// Version), and Verify both confirms no drift against that same baseline and
// detects drift against a different one. This is the live-registry coverage
// the unit tests (kind_test.go) deliberately leave out.
func TestResolve_LiveDigestFromRegistry(t *testing.T) {
	withPlainHTTPFakeRegistry(t)

	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")

	bundle, err := oapbundle.FromFolder(oaptest.WriteBundle(t))
	require.NoError(t, err)
	packed, err := oapbundle.Pack(bundle)
	require.NoError(t, err)

	tagRef := host + "/demo-agent:v1"
	pushedDigest, err := ociagent.Push(context.Background(), tagRef, packed, ociagent.Options{PlainHTTP: true})
	require.NoError(t, err)

	k := &Kind{}
	ref, err := k.ParseRef(tagRef)
	require.NoError(t, err)
	assert.Equal(t, pinning.StrengthNamed, ref.Strength, "a tag ref must classify as named")

	frozen, err := k.Resolve(context.Background(), ref)
	require.NoError(t, err)
	assert.Equal(t, pushedDigest, frozen.Digest, "Resolve must report the digest actually pushed")
	assert.Equal(t, "v1", frozen.Version, "Resolve must record the tag as Version")

	rep, err := k.Verify(context.Background(), ref, frozen)
	require.NoError(t, err)
	assert.False(t, rep.Drifted, "verifying against the just-resolved baseline must report no drift")

	staleBaseline := pinning.Frozen{Digest: "sha256:" + strings.Repeat("0", 64), Version: "v1"}
	repDrift, err := k.Verify(context.Background(), ref, staleBaseline)
	require.NoError(t, err)
	assert.True(t, repDrift.Drifted, "a baseline with a different digest must report drift")
	assert.NotEmpty(t, repDrift.Summary)
}

// TestResolve_DigestRef exercises the frozen (digest-pinned) branch: Resolve
// on an already digest-pinned ref must still return that digest and leave
// Version empty (no tag to record).
func TestResolve_DigestRef(t *testing.T) {
	withPlainHTTPFakeRegistry(t)

	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")

	bundle, err := oapbundle.FromFolder(oaptest.WriteBundle(t))
	require.NoError(t, err)
	packed, err := oapbundle.Pack(bundle)
	require.NoError(t, err)

	tagRef := host + "/demo-agent:v1"
	pushedDigest, err := ociagent.Push(context.Background(), tagRef, packed, ociagent.Options{PlainHTTP: true})
	require.NoError(t, err)

	digestRef := host + "/demo-agent@" + pushedDigest
	k := &Kind{}
	ref, err := k.ParseRef(digestRef)
	require.NoError(t, err)
	assert.Equal(t, pinning.StrengthFrozen, ref.Strength)

	frozen, err := k.Resolve(context.Background(), ref)
	require.NoError(t, err)
	assert.Equal(t, pushedDigest, frozen.Digest)
	assert.Empty(t, frozen.Version, "a digest-only ref has no tag to record as Version")
}
