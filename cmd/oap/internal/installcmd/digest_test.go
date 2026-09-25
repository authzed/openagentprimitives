package installcmd

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
)

func TestImagesMissingDigests(t *testing.T) {
	full := map[string]string{}
	for _, im := range apimage.Catalog() {
		full[im.Name] = "sha256:deadbeef"
	}
	allButToolchains := map[string]string{}
	for _, im := range apimage.All {
		allButToolchains[im.Name] = "sha256:deadbeef"
	}

	cases := []struct {
		name string
		have map[string]string
		want int
	}{
		{name: "nil map: every catalog image is missing", have: nil, want: len(apimage.Catalog())},
		{name: "fully populated: nothing missing", have: full, want: 0},
		{name: "build covered All only: exactly the toolchains are missing", have: allButToolchains, want: len(apimage.Toolchains)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Len(t, imagesMissingDigests(tc.have), tc.want)
		})
	}
}

// TestResolveMissingRegistryDigests_QueriesOnlyTheMissing proves the merge is
// additive and cheap: digests oap init already collected from its own build are
// carried through untouched and never re-queried from the registry.
func TestResolveMissingRegistryDigests_QueriesOnlyTheMissing(t *testing.T) {
	old := imagetoolsDigest
	t.Cleanup(func() { imagetoolsDigest = old })
	var queried []string
	imagetoolsDigest = func(_ context.Context, ref string) (string, error) {
		queried = append(queried, ref)
		return "sha256:resolved", nil
	}

	have := map[string]string{}
	for _, im := range apimage.All {
		have[im.Name] = "sha256:frombuild"
	}

	got, err := resolveMissingRegistryDigests(context.Background(), "myreg.io/ap", have)
	require.NoError(t, err)
	assert.Len(t, got, len(apimage.Catalog()), "the merged map covers the whole catalog")
	assert.Equal(t, "sha256:frombuild", got[apimage.Runner.Name], "a build-supplied digest is preserved")
	assert.Len(t, queried, len(apimage.Toolchains), "only the images missing a digest are queried")
	for _, im := range apimage.Toolchains {
		assert.Equal(t, "sha256:resolved", got[im.Name])
	}
}

// TestResolveMissingRegistryDigests_FailsClosedNamingTheBuildCommand is the
// guard for the bug this plan fixes. An image the install is about to plant a
// reference to, but which is not in the registry, must abort the install with
// the exact command that provisions it — not degrade to a mutable tag and
// resurface as Init:ErrImagePull inside a session days later.
func TestResolveMissingRegistryDigests_FailsClosedNamingTheBuildCommand(t *testing.T) {
	old := imagetoolsDigest
	t.Cleanup(func() { imagetoolsDigest = old })
	imagetoolsDigest = func(_ context.Context, ref string) (string, error) {
		if strings.Contains(ref, apimage.ToolchainClaude.Name) {
			return "", fmt.Errorf("not found")
		}
		return "sha256:resolved", nil
	}

	_, err := resolveMissingRegistryDigests(context.Background(), "myreg.io/ap", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "myreg.io/ap/"+apimage.ToolchainClaude.Name+":")
	assert.Contains(t, err.Error(), "oap build "+apimage.ToolchainClaude.Target+" --image-registry myreg.io/ap")
	// --no-digest-pin routes through verifyRegistryRefs, which fails on this same
	// ref for this same reason, so the hint must say so rather than offer it as an
	// escape and walk the operator into a second wall.
	assert.Contains(t, err.Error(), "--no-digest-pin does not bypass this")
}

func TestResolveMissingRegistryDigests_AllResolved(t *testing.T) {
	old := imagetoolsDigest
	t.Cleanup(func() { imagetoolsDigest = old })
	imagetoolsDigest = func(_ context.Context, ref string) (string, error) {
		return "sha256:" + ref, nil // deterministic per-ref fake
	}
	m, err := resolveMissingRegistryDigests(context.Background(), "myreg.io/ap", nil)
	require.NoError(t, err)
	assert.Len(t, m, len(apimage.Catalog()), "resolveMissingRegistryDigests must pin every catalog image, including toolchains")
	assert.Equal(t, "sha256:myreg.io/ap/agentprimitives-runner:dev", m[apimage.Runner.Name])
	// Every toolchain, not just the first: an overlay that fell out of the pinned
	// set would install as a mutable tag.
	for _, im := range apimage.Toolchains {
		assert.Equalf(t, "sha256:myreg.io/ap/"+im.Name+":dev", m[im.Name],
			"toolchain image %s must be digest-pinned too", im.Name)
	}
}

func TestResolveMissingRegistryDigests_FailsLoudOnError(t *testing.T) {
	old := imagetoolsDigest
	t.Cleanup(func() { imagetoolsDigest = old })
	imagetoolsDigest = func(_ context.Context, _ string) (string, error) {
		return "", fmt.Errorf("docker: command not found")
	}
	_, err := resolveMissingRegistryDigests(context.Background(), "myreg.io/ap", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not resolve a digest")
	assert.Contains(t, err.Error(), "--no-digest-pin does not bypass this")
}

func TestResolveMissingRegistryDigests_RejectsNonSha256(t *testing.T) {
	old := imagetoolsDigest
	t.Cleanup(func() { imagetoolsDigest = old })
	imagetoolsDigest = func(_ context.Context, _ string) (string, error) {
		return "not-a-digest", nil
	}
	_, err := resolveMissingRegistryDigests(context.Background(), "myreg.io/ap", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected digest")
}

// TestVerifyRegistryRefs proves --no-digest-pin still refuses to plant a
// reference to an image that is not in the registry. The flag opts out of
// immutability, not out of the image existing.
func TestVerifyRegistryRefs(t *testing.T) {
	cases := []struct {
		name    string
		digest  func(ctx context.Context, ref string) (string, error)
		wantErr string
	}{
		{
			name:   "every ref resolves: no error",
			digest: func(_ context.Context, _ string) (string, error) { return "sha256:ok", nil },
		},
		{
			name: "a missing overlay: error names the image and the build command",
			digest: func(_ context.Context, ref string) (string, error) {
				if strings.Contains(ref, apimage.ToolchainNode.Name) {
					return "", fmt.Errorf("not found")
				}
				return "sha256:ok", nil
			},
			wantErr: "oap build " + apimage.ToolchainNode.Target + " --image-registry myreg.io/ap",
		},
		{
			// An empty digest with no error is what a drifted buildx output shape
			// produces: parseImagetoolsManifestDigest unmarshals `{}` cleanly and
			// returns ("", nil). Without the sha256: check the whole verification
			// pass would silently succeed for every image while proving nothing.
			name:    "empty digest, no error: rejected rather than passing vacuously",
			digest:  func(_ context.Context, _ string) (string, error) { return "", nil },
			wantErr: "unexpected digest",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := imagetoolsDigest
			t.Cleanup(func() { imagetoolsDigest = old })
			imagetoolsDigest = tc.digest

			err := verifyRegistryRefs(context.Background(), "myreg.io/ap")
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestInstallRegistryDryRun_FailsClosedOnAnUnpushedToolchainImage covers the
// CALL SITE, not the helpers. The outage this branch fixes was a single gate
// condition in RunInstall (`&& len(tags.Digests) == 0`) that skipped the
// fail-closed resolve entirely; every helper it guarded was correct in
// isolation. Driving `install --image-registry` end-to-end through the root
// command is what makes re-adding such a short-circuit turn a test red.
//
// --dry-run=client is safe here and touches no cluster: it skips the bundle /
// cloud-detect block above the digest gate, and returns before any apply. The
// digest gate itself runs regardless of dry-run, which is precisely the
// behavior under test.
func TestInstallRegistryDryRun_FailsClosedOnAnUnpushedToolchainImage(t *testing.T) {
	old := imagetoolsDigest
	t.Cleanup(func() { imagetoolsDigest = old })
	imagetoolsDigest = func(_ context.Context, ref string) (string, error) {
		if strings.Contains(ref, apimage.ToolchainGo.Name) {
			return "", fmt.Errorf("not found")
		}
		return "sha256:resolved", nil
	}

	root := newRoot(t)
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stdout)
	root.SetArgs([]string{"install", "--dry-run=client", "--image-registry", "myreg.io/ap"})
	err := root.Execute()

	require.Error(t, err, "install must abort when an image it is about to plant is not in the registry")
	assert.Contains(t, err.Error(), "myreg.io/ap/"+apimage.ToolchainGo.Name+":", "the error names the unresolvable image ref")
	assert.Contains(t, err.Error(), "oap build "+apimage.ToolchainGo.Target+" --image-registry myreg.io/ap", "the error names the command that provisions it")
	assert.NotContains(t, stdout.String(), "(would apply)", "the install must not reach the dry-run apply listing")
}

// TestParseImagetoolsManifestDigest pins the JSON-descriptor parsing used by
// imagetoolsDigest. Current Docker Desktop buildx silently ignores the older
// `--format "{{.Manifest.Digest}}"` template and prints its default
// human-readable blob instead, which the resolver then mistook for a digest. We
// now ask for `{{json .Manifest}}` and read the descriptor's top-level digest,
// which is correct for both a single-platform manifest and a multi-arch index.
func TestParseImagetoolsManifestDigest(t *testing.T) {
	const sha = "sha256:fffd43be2d9490911dbed66dd520e998c547a29075748ba31019a6f1aa18b1a4"
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{
			name: "single-platform v2 manifest descriptor",
			in:   `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","digest":"` + sha + `","size":1862}`,
			want: sha,
		},
		{
			name: "multi-arch OCI index descriptor",
			in:   `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","digest":"` + sha + `","size":8077,"manifests":[{"digest":"sha256:dead"}]}`,
			want: sha,
		},
		{
			name: "surrounding whitespace tolerated",
			in:   "\n  " + `{"digest":"` + sha + `"}` + "  \n",
			want: sha,
		},
		{
			name:    "default human-readable blob (old --format bug) is rejected, not parsed as a digest",
			in:      "Name:      reg/img:dev\nMediaType: application/vnd.docker.distribution.manifest.v2+json\nDigest:    " + sha,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseImagetoolsManifestDigest([]byte(tc.in))
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// stubLocalImagePresent swaps the local-daemon probe for the test and restores
// it on cleanup. Tests using it MUST NOT run in parallel (package-global state).
func stubLocalImagePresent(t *testing.T, present map[string]bool) {
	t.Helper()
	prev := localImagePresent
	localImagePresent = func(_ context.Context, ref string) bool { return present[ref] }
	t.Cleanup(func() { localImagePresent = prev })
}

// TestWarnMissingLocalToolchainImages covers the local-cluster half of the
// dangling-reference bug. A local install plants unqualified ap-toolchain-*:dev
// refs into the SpiceboxToolchain CRs, but `oap build all` deliberately expands
// over apimage.All only — so unlike the registry path, nothing provisions the
// overlays and nothing notices. We cannot prove what is in the node's image
// store from here, but absence from the local daemon does prove the overlay was
// never built to load, which is the case worth warning about.
func TestWarnMissingLocalToolchainImages(t *testing.T) {
	allPresent := func() map[string]bool {
		m := map[string]bool{}
		for _, im := range apimage.Toolchains {
			m[im.LocalRef()] = true
		}
		return m
	}
	without := func(missing apimage.Image) map[string]bool {
		m := allPresent()
		delete(m, missing.LocalRef())
		return m
	}

	cases := []struct {
		name         string
		present      map[string]bool
		kctx         string
		wantSilent   bool
		wantContains []string
	}{
		{
			name:       "every overlay present locally: silent",
			present:    allPresent(),
			kctx:       "kind-ap",
			wantSilent: true,
		},
		{
			name:    "one overlay missing on kind: names it, the build command, and the load command",
			present: without(apimage.ToolchainGo),
			kctx:    "kind-ap",
			wantContains: []string{
				apimage.ToolchainGo.LocalRef(),
				"oap build " + apimage.ToolchainGo.Target,
				"kind load docker-image " + apimage.ToolchainGo.LocalRef() + " --name ap",
			},
		},
		{
			name:    "missing on minikube: renders that cluster's load command",
			present: without(apimage.ToolchainNode),
			kctx:    "minikube",
			wantContains: []string{
				"oap build " + apimage.ToolchainNode.Target,
				"minikube image load " + apimage.ToolchainNode.LocalRef(),
			},
		},
		{
			name:    "all overlays missing: one warning block, every overlay named",
			present: map[string]bool{},
			kctx:    "kind-ap",
			wantContains: []string{
				apimage.ToolchainGo.Target, apimage.ToolchainNode.Target, apimage.ToolchainClaude.Target,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubLocalImagePresent(t, tc.present)
			var buf bytes.Buffer
			warnMissingLocalToolchainImages(context.Background(), &buf, tc.kctx)
			if tc.wantSilent {
				assert.Empty(t, buf.String(), "no warning when every overlay is already built")
				return
			}
			for _, want := range tc.wantContains {
				assert.Containsf(t, buf.String(), want, "warning must mention %q", want)
			}
		})
	}
}
