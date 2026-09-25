//go:build integration

package oci_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	ociagent "github.com/authzed/openagentprimitives/pkg/platform/oap/oci"
	"github.com/authzed/openagentprimitives/test/oaptest"
)

// TestSignVerify_RoundTrip stands up the same in-process, spec-compliant
// registry TestPushPull_RoundTrip uses, Pushes a packed demo-agent .oap, Signs
// it with a generated ECDSA-P256 key, and asserts:
//
//   - Verify with the matching public key passes AND returns the pushed
//     manifest digest (the identity a caller can pin to).
//   - Verify with a different public key fails closed with ErrNoValidSignature.
//   - After re-Pushing different bytes to the same ref (tampering
//     post-signature), Verify fails closed too — because Verify always resolves
//     ref's *current* manifest digest first, a stale signature naming the old
//     digest is simply invisible to it, not merely "cryptographically wrong".
//
// It runs the whole scenario twice, once with the registry's native OCI 1.1
// Referrers API enabled and once without. With it disabled the registry answers
// GET /v2/<name>/referrers/<digest> as not-supported, forcing Verify down
// oras-go's client-side referrers-tag-schema fallback. That path needs its own
// case because our signature manifest declares its artifactType via
// config.mediaType with no top-level `artifactType` field, and oras's fallback
// index derives ArtifactType from config.mediaType only when the top-level
// field is empty — if that derivation changed, Verify would silently stop
// finding fallback-mode signatures.
func TestSignVerify_RoundTrip(t *testing.T) {
	for _, referrersSupport := range []bool{true, false} {
		name := "native OCI 1.1 Referrers API"
		if !referrersSupport {
			name = "tag-schema fallback (no native Referrers API)"
		}
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(registry.New(registry.WithReferrersSupport(referrersSupport)))
			t.Cleanup(srv.Close)
			host := strings.TrimPrefix(srv.URL, "http://")

			bundle, err := oap.FromFolder(oaptest.WriteBundle(t))
			require.NoError(t, err)
			packed, err := oap.Pack(bundle)
			require.NoError(t, err)

			ref := host + "/demo-agent:v1"
			opts := ociagent.Options{PlainHTTP: true}
			ctx := context.Background()

			pushDigest, err := ociagent.Push(ctx, ref, packed, opts)
			require.NoError(t, err)

			priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			require.NoError(t, err)

			sigDigest, err := ociagent.Sign(ctx, ref, priv, opts)
			require.NoError(t, err)
			assert.NotEmpty(t, sigDigest)

			t.Run("matching public key verifies and returns the pushed digest", func(t *testing.T) {
				verifiedDigest, err := ociagent.Verify(ctx, ref, priv.Public(), opts)
				require.NoError(t, err)
				assert.Equal(t, pushDigest, verifiedDigest, "Verify must return the exact manifest digest it validated")
			})

			t.Run("wrong public key fails closed", func(t *testing.T) {
				wrongPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				require.NoError(t, err)
				verifiedDigest, err := ociagent.Verify(ctx, ref, wrongPriv.Public(), opts)
				require.Error(t, err)
				assert.ErrorIs(t, err, ociagent.ErrNoValidSignature)
				assert.Empty(t, verifiedDigest, "a failed Verify must not return a digest")
			})

			t.Run("tampered manifest (re-pushed under the same ref) fails closed", func(t *testing.T) {
				tampered := *bundle
				tamperedAssets := make(map[string][]byte, len(bundle.Assets)+1)
				for k, v := range bundle.Assets {
					tamperedAssets[k] = v
				}
				tamperedAssets["tamper-marker.txt"] = []byte("this bundle was tampered with after signing")
				tampered.Assets = tamperedAssets

				tamperedPacked, err := oap.Pack(&tampered)
				require.NoError(t, err)
				tamperedDigest, err := oap.Digest(tamperedPacked)
				require.NoError(t, err)
				localDigest, err := oap.Digest(packed)
				require.NoError(t, err)
				require.NotEqual(t, localDigest, tamperedDigest, "test fixture must actually produce a different digest")

				_, err = ociagent.Push(ctx, ref, tamperedPacked, opts)
				require.NoError(t, err)

				_, err = ociagent.Verify(ctx, ref, priv.Public(), opts)
				require.Error(t, err)
				assert.ErrorIs(t, err, ociagent.ErrNoValidSignature)
			})
		})
	}
}

// TestVerifiedDigestPin_DefeatsTagTOCTOU proves the reason Verify returns the
// verified digest: pinning a subsequent pull to it closes the tag-mutation
// TOCTOU gap. It signs content A under tag :v1, then moves :v1 to a different
// (unsigned) content B — exactly what a hostile registry serving signed-A to
// verify and unsigned-B to pull would do. It then asserts that a pull pinned
// to the verified digest (via PinnedRef) still fetches A, while a pull of the
// bare tag now fetches B — so a verify-then-pull that pins is provably immune
// to the tag having moved in between.
func TestVerifiedDigestPin_DefeatsTagTOCTOU(t *testing.T) {
	srv := httptest.NewServer(registry.New(registry.WithReferrersSupport(true)))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")

	bundle, err := oap.FromFolder(oaptest.WriteBundle(t))
	require.NoError(t, err)
	packedA, err := oap.Pack(bundle)
	require.NoError(t, err)

	ref := host + "/demo-agent:v1"
	opts := ociagent.Options{PlainHTTP: true}
	ctx := context.Background()

	digestA, err := ociagent.Push(ctx, ref, packedA, opts)
	require.NoError(t, err)

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	_, err = ociagent.Sign(ctx, ref, priv, opts)
	require.NoError(t, err)

	// Verify resolves :v1 -> A and returns A's digest.
	verifiedDigest, err := ociagent.Verify(ctx, ref, priv.Public(), opts)
	require.NoError(t, err)
	require.Equal(t, digestA, verifiedDigest)

	// Now move the mutable tag :v1 to different, UNSIGNED content B.
	tampered := *bundle
	tamperedAssets := make(map[string][]byte, len(bundle.Assets)+1)
	for k, v := range bundle.Assets {
		tamperedAssets[k] = v
	}
	tamperedAssets["tamper-marker.txt"] = []byte("swapped in after verify")
	tampered.Assets = tamperedAssets
	packedB, err := oap.Pack(&tampered)
	require.NoError(t, err)
	digestB, err := ociagent.Push(ctx, ref, packedB, opts)
	require.NoError(t, err)
	require.NotEqual(t, digestA, digestB, "test fixture must move the tag to different content")

	// A pull of the bare tag now yields B — the tag genuinely moved.
	_, pulledTagDigest, err := ociagent.Pull(ctx, ref, opts)
	require.NoError(t, err)
	assert.Equal(t, digestB, pulledTagDigest, "bare-tag pull follows the moved tag to B")

	// A pull pinned to the verified digest still yields A — TOCTOU defeated.
	pinnedRef, err := ociagent.PinnedRef(ref, verifiedDigest)
	require.NoError(t, err)
	_, pulledPinnedDigest, err := ociagent.Pull(ctx, pinnedRef, opts)
	require.NoError(t, err)
	assert.Equal(t, digestA, pulledPinnedDigest, "digest-pinned pull fetches the verified manifest A, not the moved tag's B")
}

// TestSignVerify_CosignCLIInterop is the real-CLI interop check: when a `cosign`
// binary is on PATH it shells out to `cosign verify --key
// --registry-referrers-mode=oci-1-1` against a signature produced by Sign,
// proving byte-for-byte compatibility with cosign's own OCI-1.1-referrers
// format rather than just our Verify agreeing with our Sign. If cosign is
// absent, or present but too old to know `--registry-referrers-mode` (this
// format is an OCI-1.1-only addition), the test skips with a logged note rather
// than failing — cosign's presence is not something this suite gates on.
func TestSignVerify_CosignCLIInterop(t *testing.T) {
	cosignPath, err := exec.LookPath("cosign")
	if err != nil {
		t.Skip("cosign binary not found on PATH; skipping real-CLI interop check")
	}

	srv := httptest.NewServer(registry.New(registry.WithReferrersSupport(true)))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")

	bundle, err := oap.FromFolder(oaptest.WriteBundle(t))
	require.NoError(t, err)
	packed, err := oap.Pack(bundle)
	require.NoError(t, err)

	ref := host + "/demo-agent:v1"
	opts := ociagent.Options{PlainHTTP: true}
	ctx := context.Background()

	_, err = ociagent.Push(ctx, ref, packed, opts)
	require.NoError(t, err)

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	_, err = ociagent.Sign(ctx, ref, priv, opts)
	require.NoError(t, err)

	derBytes, err := x509.MarshalPKIXPublicKey(priv.Public())
	require.NoError(t, err)
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: derBytes})

	dir := t.TempDir()
	pubPath := filepath.Join(dir, "cosign.pub")
	require.NoError(t, os.WriteFile(pubPath, pubPEM, 0o600))

	cmd := exec.Command(cosignPath, "verify",
		"--key", pubPath,
		"--insecure-ignore-tlog",
		"--allow-insecure-registry",
		"--registry-referrers-mode=oci-1-1",
		ref,
	)
	out, err := cmd.CombinedOutput()
	if err != nil && strings.Contains(string(out), "unknown flag") {
		t.Skipf("installed cosign binary does not support --registry-referrers-mode (too old for OCI 1.1 referrers-mode verify); skipping real-CLI interop.\ncosign output: %s", out)
	}
	assert.NoError(t, err, "cosign verify --registry-referrers-mode=oci-1-1 should accept our signature; output:\n%s", out)
}
