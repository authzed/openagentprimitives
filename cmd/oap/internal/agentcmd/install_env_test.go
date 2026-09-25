package agentcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/imagebuild"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/imageload"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

func TestResolveBuildSecret(t *testing.T) {
	t.Setenv("MY_TOK", "fromenv")
	v, err := resolveBuildSecret("github_token", map[string]string{"github_token": "env:MY_TOK"})
	require.NoError(t, err)
	assert.Equal(t, "fromenv", v)

	v, err = resolveBuildSecret("github_token", map[string]string{"github_token": "literalvalue"})
	require.NoError(t, err)
	assert.Equal(t, "literalvalue", v)

	_, err = resolveBuildSecret("github_token", map[string]string{"github_token": "env:UNSET_VAR_XYZ_9137"})
	assert.ErrorContains(t, err, "UNSET_VAR_XYZ_9137")
}

// A remote context with no registry resolvable and no TTY must fail closed at
// env construction — BEFORE any image is built — rather than building locally
// and reporting success for an image the cluster can never pull.
func TestNewInstallEnvRemoteWithoutRegistryFailsClosed(t *testing.T) {
	g := &apcmd.Globals{Context: "gke_my-proj_us-east1_prod"}
	g.BundleFn = func() (*kube.Bundle, error) {
		return &kube.Bundle{Typed: fake.NewSimpleClientset(aptest.NodeWithProviderID("aws:///us-west-2a/i-0abc"))}, nil
	}
	_, err := newInstallEnv(context.Background(), g, t.TempDir(), buildSets{}, io.Discard, nil /*no driver: nobody at stdin*/, nil)
	require.Error(t, err)
	assert.ErrorContains(t, err, "image registry")
}

// The registry is read off the installed operator image, so a remote install
// needs neither a flag nor a prompt.
func TestNewInstallEnvRemoteInfersRegistryFromOperator(t *testing.T) {
	const installed = "us-east1-docker.pkg.dev/my-proj/ap"
	g := &apcmd.Globals{Context: "gke_my-proj_us-east1_prod"}
	g.BundleFn = func() (*kube.Bundle, error) {
		return &kube.Bundle{Typed: fake.NewSimpleClientset(
			aptest.OperatorDeployment(installed+"/spicebox-operator:v1@sha256:abc"),
			aptest.NodeWithProviderID("gce://my-proj/us-east1-c/n"),
		)}, nil
	}
	env, err := newInstallEnv(context.Background(), g, t.TempDir(), buildSets{}, io.Discard, nil, nil)
	require.NoError(t, err)

	ie, ok := env.(*installEnv)
	require.True(t, ok, "newInstallEnv must return *installEnv")
	assert.Equal(t, imageload.NeedsRegistry, ie.disp)
	assert.Equal(t, installed, ie.registry)
}

// A local kind context keeps the load path and resolves no registry at all.
func TestNewInstallEnvLocalContextNeedsNoRegistry(t *testing.T) {
	g := &apcmd.Globals{Context: "kind-mycluster"}
	g.BundleFn = func() (*kube.Bundle, error) {
		return &kube.Bundle{Typed: fake.NewSimpleClientset()}, nil
	}
	env, err := newInstallEnv(context.Background(), g, t.TempDir(), buildSets{}, io.Discard, nil, nil)
	require.NoError(t, err)

	ie, ok := env.(*installEnv)
	require.True(t, ok, "newInstallEnv must return *installEnv")
	assert.Equal(t, imageload.LocalLoad, ie.disp)
	assert.Empty(t, ie.registry)
}

// An explicit --image-registry IS the answer, so newInstallEnv must not reach
// for the cluster to re-derive one. A namespace-scoped kubeconfig cannot list
// nodes, and cloud.Detect's failure there used to defeat an invocation that had
// already supplied everything needed.
func TestNewInstallEnvExplicitRegistrySkipsClusterReads(t *testing.T) {
	const reg = "myreg.example.com/team/ap"
	g := &apcmd.Globals{Context: "gke_my-proj_us-east1_prod"}
	bundleCalls := 0
	g.BundleFn = func() (*kube.Bundle, error) {
		bundleCalls++
		return nil, errors.New(`nodes is forbidden: User "svc" cannot list resource "nodes"`)
	}

	env, err := newInstallEnv(context.Background(), g, t.TempDir(), buildSets{registry: reg}, io.Discard, nil, nil)
	require.NoError(t, err, "an explicit --image-registry must resolve without a cluster")
	assert.Zero(t, bundleCalls, "no cluster connection may be opened when the registry is given")

	ie, ok := env.(*installEnv)
	require.True(t, ok, "newInstallEnv must return *installEnv")
	assert.Equal(t, imageload.NeedsRegistry, ie.disp)
	assert.Equal(t, reg, ie.registry)
}

// MirrorRef is the pure name-mapping step: bundle ref -> the same image under
// the cluster's registry. It stays tag-shaped because it is what gets PUSHED to
// and probed; pinning is a separate, later step performed by Resolve/Deliver
// against what the registry actually holds.
func TestMirrorRefCarriesNoDigest(t *testing.T) {
	const reg = "us-east1-docker.pkg.dev/my-proj/ap"
	got := apimage.MirrorRef("demo-image:dev", reg)
	assert.Equal(t, "us-east1-docker.pkg.dev/my-proj/ap/demo-image:dev", got)
	assert.NotContains(t, got, "@sha256:", "a per-build digest must never reach the applied ref")
}

// fakeRegistryHost starts an in-process OCI registry (go-containerregistry's
// own reference implementation) and returns its host:port. Using a real
// listener (rather than mocking name/remote) exercises registryHasImage's
// actual HTTP probe — the part of the push path that isn't gated behind a
// docker/buildx subprocess — over plain HTTP, since name.ParseReference
// auto-detects 127.0.0.1 as insecure.
func fakeRegistryHost(t *testing.T) string {
	t.Helper()
	quiet := log.New(io.Discard, "", 0)
	srv := httptest.NewServer(registry.New(registry.Logger(quiet)))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// pushRandomImage pushes a small random image to ref (must resolve against a
// reachable registry), so a presence probe against it has something real to
// find. It returns the pushed digest, which is what the probe must report back
// for the resolved ref to be pinnable.
func pushRandomImage(t *testing.T, ref string) string {
	t.Helper()
	r, err := name.ParseReference(ref)
	require.NoError(t, err)
	img, err := random.Image(1024, 1)
	require.NoError(t, err)
	require.NoError(t, remote.Write(r, img))
	dig, err := img.Digest()
	require.NoError(t, err)
	return dig.String()
}

// registryImageDigest is evidence-gathering, not a subprocess wrapper: it talks
// straight HTTP to the registry, so it can be exercised against a real
// in-process registry without any docker/buildx dependency. The same HEAD that
// answers "is it there?" also carries the digest, so presence and identity are
// one round-trip — the caller never has to trust a tag it already resolved.
func TestRegistryImageDigest(t *testing.T) {
	host := fakeRegistryHost(t)

	t.Run("absent image: HEAD 404 -> absent, no digest, no error", func(t *testing.T) {
		dig, present, err := registryImageDigest(context.Background(), host+"/demo/absent:dev")
		require.NoError(t, err)
		assert.False(t, present)
		assert.Empty(t, dig, "an absent image has no digest to report")
	})

	t.Run("present image: pushed then probed -> present with the pushed digest", func(t *testing.T) {
		ref := host + "/demo/present:dev"
		want := pushRandomImage(t, ref)
		dig, present, err := registryImageDigest(context.Background(), ref)
		require.NoError(t, err)
		assert.True(t, present)
		assert.Equal(t, want, dig, "the probe must report the digest the registry actually holds")
	})

	t.Run("malformed ref -> parse error", func(t *testing.T) {
		_, _, err := registryImageDigest(context.Background(), "not a valid ref ::")
		assert.Error(t, err)
	})

	t.Run("unreachable registry -> error propagated (not swallowed as absent)", func(t *testing.T) {
		// Nothing listens on this loopback port: the HEAD fails at the
		// transport level (connection refused), never producing an HTTP status,
		// so it must NOT be classified as "definitively absent" the way a 404 is.
		_, _, err := registryImageDigest(context.Background(), "127.0.0.1:1/demo/x:dev")
		assert.Error(t, err)
	})
}

// installEnv.Resolve's NeedsRegistry branch must mirror the ref into the
// resolved registry and report real presence there — and when the image IS
// present it must hand back a DIGEST-pinned ref, not the mutable tag it probed.
//
// This is the fix for a session that failed after its image was replaced: the
// class recorded ".../sre-sandbox:dev", the node had that tag cached from an
// earlier push, and nothing in the pod spec forces a re-pull, so the node kept
// running the superseded (wrong-arch) layers. A digest ref is content-addressed,
// so a stale node cache cannot satisfy it.
func TestInstallEnvResolveNeedsRegistryPinsPresentImageByDigest(t *testing.T) {
	host := fakeRegistryHost(t)
	e := &installEnv{out: io.Discard, disp: imageload.NeedsRegistry, registry: host + "/demo"}

	t.Run("absent: mirrored tag returned, nothing to pin yet", func(t *testing.T) {
		resolved, present, err := e.Resolve(context.Background(), "absent:dev")
		require.NoError(t, err)
		assert.Equal(t, host+"/demo/absent:dev", resolved)
		assert.False(t, present)
	})

	t.Run("present: resolved to the registry's digest, not the tag", func(t *testing.T) {
		dig := pushRandomImage(t, host+"/demo/present:dev")
		resolved, present, err := e.Resolve(context.Background(), "present:dev")
		require.NoError(t, err)
		assert.True(t, present)
		assert.Equal(t, host+"/demo/present@"+dig, resolved)
		assert.NotContains(t, resolved, ":dev", "the mutable tag must not survive into the applied ref")
	})
}

// The property that makes client-side pinning safe for server-side apply:
// resolving an UNCHANGED tag twice yields a byte-identical ref, so a re-install
// stays an SSA no-op. The digest comes from probing the registry's current
// tag — not from a fresh (non-reproducible) build — which is exactly why this
// can be applied where a per-build digest could not.
func TestInstallEnvResolveIsStableAcrossReinstalls(t *testing.T) {
	host := fakeRegistryHost(t)
	e := &installEnv{out: io.Discard, disp: imageload.NeedsRegistry, registry: host + "/demo"}
	pushRandomImage(t, host+"/demo/stable:dev")

	first, present, err := e.Resolve(context.Background(), "stable:dev")
	require.NoError(t, err)
	require.True(t, present)

	second, present, err := e.Resolve(context.Background(), "stable:dev")
	require.NoError(t, err)
	require.True(t, present)

	assert.Equal(t, first, second, "an unchanged tag must resolve identically, or re-install stops being an SSA no-op")
}

// Local delivery paths put the image straight onto the node (or share the
// laptop's daemon); the ref never goes through a registry, so there is no digest
// to resolve and pinning one would produce a ref the node cannot match. These
// paths must keep handing back the bare tag.
func TestInstallEnvResolveLocalPathsKeepTheBareTag(t *testing.T) {
	for _, disp := range []imageload.Disposition{imageload.LocalLoad, imageload.NotNeeded} {
		t.Run(disp.String()+": bare tag, reported absent", func(t *testing.T) {
			e := &installEnv{out: io.Discard, disp: disp}
			resolved, present, err := e.Resolve(context.Background(), "demo-image:dev")
			require.NoError(t, err)
			assert.Equal(t, "demo-image:dev", resolved)
			assert.False(t, present)
		})
	}
}

// A probe failure (unreachable registry here; the same contract covers no
// local credentials or a 401) must not block Reconcile from proceeding to a
// build+push — but per AGENTS.md's no-silent-errors rule it must be surfaced
// on out, not swallowed.
func TestInstallEnvResolveNeedsRegistryProbeFailureWarnsAndTreatsAbsent(t *testing.T) {
	var out bytes.Buffer
	e := &installEnv{out: &out, disp: imageload.NeedsRegistry, registry: "127.0.0.1:1/demo"}

	resolved, present, err := e.Resolve(context.Background(), "x:dev")
	require.NoError(t, err, "a probe failure must not block reconcile")
	assert.False(t, present)
	assert.Equal(t, "127.0.0.1:1/demo/x:dev", resolved)
	assert.Contains(t, out.String(), "could not check whether", "the probe failure must be surfaced, not silent")
}

// The presence probe and `docker buildx --push` share the ambient Docker
// credential helpers, so a 401/403 from the probe predicts a failed push. It
// must be classified apart from "absent" (404) and "unreachable" (no HTTP
// status), so Resolve can say so before the user waits out a cross-build.
func TestAuthRejection(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantAuth   bool
	}{
		{name: "401: auth refusal, status reported", err: &transport.Error{StatusCode: http.StatusUnauthorized}, wantStatus: 401, wantAuth: true},
		{name: "403: auth refusal, status reported", err: &transport.Error{StatusCode: http.StatusForbidden}, wantStatus: 403, wantAuth: true},
		{name: "404: absent, not an auth refusal", err: &transport.Error{StatusCode: http.StatusNotFound}, wantAuth: false},
		{name: "500: registry fault, not an auth refusal", err: &transport.Error{StatusCode: http.StatusInternalServerError}, wantAuth: false},
		{name: "wrapped 403: unwrapped and reported", err: fmt.Errorf("head demo: %w", &transport.Error{StatusCode: http.StatusForbidden}), wantStatus: 403, wantAuth: true},
		{name: "non-HTTP error (unreachable): not an auth refusal", err: errors.New("connection refused"), wantAuth: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, isAuth := authRejection(tc.err)
			assert.Equal(t, tc.wantAuth, isAuth)
			assert.Equal(t, tc.wantStatus, status)
		})
	}
}

// A registry that refuses the credentials must produce the auth-specific
// warning (naming the remedy) rather than the generic probe-failed one — while
// still continuing to the build+push, since the probe is evidence, not proof.
func TestInstallEnvResolveAuthRejectionWarnsWithRemedyAndContinues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Advertise basic auth so go-containerregistry's transport completes
		// its ping, then refuse the anonymous request the same way a real
		// registry refuses an unauthenticated pull.
		w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")

	var out bytes.Buffer
	e := &installEnv{out: &out, disp: imageload.NeedsRegistry, registry: host + "/demo"}

	resolved, present, err := e.Resolve(context.Background(), "x:dev")
	require.NoError(t, err, "an auth refusal must not block reconcile")
	assert.False(t, present)
	assert.Equal(t, host+"/demo/x:dev", resolved)
	assert.Contains(t, out.String(), "refused the credentials", "an auth refusal must be named as such")
	assert.Contains(t, out.String(), "buildx --push", "the warning must say the push will hit the same wall")
	assert.NotContains(t, out.String(), "could not check whether", "the generic probe-failed wording must not be used for an auth refusal")
}

// nodeWithArch is gkeNode plus a reported architecture, for the push-platform
// tests: the registry path must target the CLUSTER's arch, not the laptop's.
func nodeWithArch(providerID, arch string) *corev1.Node {
	n := aptest.NodeWithProviderID(providerID)
	n.Status.NodeInfo.Architecture = arch
	return n
}

// A registry push targets the cluster's NODES, not the machine running ap.
// --platform defaulting to the docker host arch is right for a local load (the
// node IS the host) and silently wrong for a push: on an arm64 laptop it
// produced an arm64 image for amd64 nodes, which pushes and pulls cleanly and
// then dies with "exec format error" the first time a container starts.
// `oap init` already resolves this via detectNodeArch; `oap agent install` did not.
func TestNewInstallEnvPushPlatformTargetsNodeArch(t *testing.T) {
	const installed = "us-east1-docker.pkg.dev/my-proj/ap"
	cases := []struct {
		name         string
		nodeArch     string
		flagPlatform string
		want         string
	}{
		{
			name:     "amd64 nodes, no --platform: builds linux/amd64, not the host arch",
			nodeArch: "amd64",
			want:     "linux/amd64",
		},
		{
			name:     "arm64 nodes, no --platform: builds linux/arm64",
			nodeArch: "arm64",
			want:     "linux/arm64",
		},
		{
			name:         "explicit --platform wins over the detected node arch",
			nodeArch:     "amd64",
			flagPlatform: "linux/arm64",
			want:         "linux/arm64",
		},
		{
			name:     "undetectable node arch: falls back to linux/amd64, never the host",
			nodeArch: "",
			want:     "linux/amd64",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &apcmd.Globals{Context: "gke_my-proj_us-east1_prod"}
			g.BundleFn = func() (*kube.Bundle, error) {
				return &kube.Bundle{Typed: fake.NewSimpleClientset(
					aptest.OperatorDeployment(installed+"/spicebox-operator:v1"),
					nodeWithArch("gce://my-proj/us-east1-c/n", tc.nodeArch),
				)}, nil
			}
			env, err := newInstallEnv(context.Background(), g, t.TempDir(),
				buildSets{platform: tc.flagPlatform}, io.Discard, nil, nil)
			require.NoError(t, err)

			ie, ok := env.(*installEnv)
			require.True(t, ok, "newInstallEnv must return *installEnv")
			assert.Equal(t, tc.want, ie.pushPlatform)
		})
	}
}

// With --image-registry supplied we deliberately never read the cluster (a
// namespace-scoped kubeconfig cannot list nodes), so node arch is undetectable.
// The default must still be linux/amd64 — the host arch is never right for a push.
func TestNewInstallEnvPushPlatformDefaultsWithoutClusterRead(t *testing.T) {
	g := &apcmd.Globals{Context: "gke_my-proj_us-east1_prod"}
	g.BundleFn = func() (*kube.Bundle, error) {
		t.Fatal("must not read the cluster when --image-registry is supplied")
		return nil, nil
	}
	env, err := newInstallEnv(context.Background(), g, t.TempDir(),
		buildSets{registry: "myreg.io/ap"}, io.Discard, nil, nil)
	require.NoError(t, err)

	ie := env.(*installEnv)
	assert.Equal(t, "linux/amd64", ie.pushPlatform)
}

// pushedDigest is the digest recordingRunner reports back through buildx's
// metadata file. It must be a syntactically valid digest, because Deliver now
// pins the returned ref to it.
const pushedDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// recordingRunner captures the docker command installEnv.Deliver builds.
type recordingRunner struct{ argv []string }

func (r *recordingRunner) Run(cmd *exec.Cmd) error {
	r.argv = cmd.Args
	for i, a := range cmd.Args {
		if a == "--metadata-file" && i+1 < len(cmd.Args) {
			_ = os.WriteFile(cmd.Args[i+1], []byte(`{"containerimage.digest":"`+pushedDigest+`"}`), 0o600)
		}
	}
	return nil
}

// The end-to-end guard for the architecture bug that reached a real cluster:
// the whole chain from the cluster's node arch through pushPlatform into the
// docker argv. Deliver is called with an EMPTY Inputs.Platform — exactly what
// `oap agent install` passes when --platform is unset — and the push must still
// target the nodes, not whatever machine is running ap.
func TestInstallEnvDeliverPushesForTheClusterArchNotTheHost(t *testing.T) {
	const installed = "us-east1-docker.pkg.dev/my-proj/ap"
	g := &apcmd.Globals{Context: "gke_my-proj_us-east1_prod"}
	g.BundleFn = func() (*kube.Bundle, error) {
		return &kube.Bundle{Typed: fake.NewSimpleClientset(
			aptest.OperatorDeployment(installed+"/spicebox-operator:v1"),
			nodeWithArch("gce://my-proj/us-east1-c/n", "amd64"),
		)}, nil
	}
	env, err := newInstallEnv(context.Background(), g, t.TempDir(), buildSets{}, io.Discard, nil, nil)
	require.NoError(t, err)

	ie := env.(*installEnv)
	r := &recordingRunner{}
	ie.runner = r

	got, err := ie.Deliver(context.Background(),
		oap.ImageBuild{Dockerfile: "images/app/Dockerfile", Context: "images/app"},
		imagebuild.Inputs{}, "demo-image:dev")
	require.NoError(t, err)

	assert.Subset(t, r.argv, []string{"--platform", "linux/amd64"},
		"an unset --platform must resolve to the cluster's node arch, never the host's")
	assert.Equal(t, installed+"/demo-image@"+pushedDigest, got,
		"Deliver returns the ref pinned to what it just pushed")
	assert.Subset(t, r.argv, []string{"-t", installed + "/demo-image:dev"},
		"the push itself still targets the tag; only the recorded ref is pinned")
}
