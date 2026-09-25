package installcmd

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/imagemode"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
)

const (
	testReg        = "registry.example.com/ap"
	testDeployed   = "sha256:5f4cd9990fe190437fbcf457e80137510724da39a6459b71650b8f25b16999cf"
	testCurrent    = "sha256:59bc10dc921934f26c32cf9a1b8010946de3279535894232c75b7f0a497ed402"
	operatorTagRef = testReg + "/spicebox-operator:dev"
)

func depWithImage(name, container, image string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: operatorNS},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: container, Image: image}},
				},
			},
		},
	}
}

// stubImagetoolsDigest swaps the package-level resolver for the test and restores
// it on cleanup. Tests using it MUST NOT run in parallel (package-global state).
func stubImagetoolsDigest(t *testing.T, fn func(ref string) (string, error)) {
	t.Helper()
	prev := imagetoolsDigest
	imagetoolsDigest = func(_ context.Context, ref string) (string, error) { return fn(ref) }
	t.Cleanup(func() { imagetoolsDigest = prev })
}

func runDrift(t *testing.T, repair bool, resolver func(ref string) (string, error), objs ...runtime.Object) (string, *fake.Clientset) {
	t.Helper()
	cs := fake.NewSimpleClientset(objs...)
	stubImagetoolsDigest(t, resolver)
	var buf bytes.Buffer
	require.NoError(t, checkImageDrift(context.Background(), &buf, &kube.Bundle{Typed: cs}, repair))
	return buf.String(), cs
}

func deployedImage(t *testing.T, cs *fake.Clientset, name string) string {
	t.Helper()
	d, err := cs.AppsV1().Deployments(operatorNS).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return d.Spec.Template.Spec.Containers[0].Image
}

func TestCheckImageDrift_UpToDate_PassesNoRepin(t *testing.T) {
	pinned := operatorTagRef + "@" + testCurrent
	out, cs := runDrift(t, true,
		func(string) (string, error) { return testCurrent, nil },
		depWithImage("spicebox-operator", "operator", pinned))

	assert.Contains(t, out, "up to date")
	assert.Equal(t, pinned, deployedImage(t, cs, "spicebox-operator"), "image must be unchanged when up to date")
}

func TestCheckImageDrift_Drift_WarnsAndDoesNotRepinWithoutRepair(t *testing.T) {
	pinned := operatorTagRef + "@" + testDeployed
	out, cs := runDrift(t, false,
		func(string) (string, error) { return testCurrent, nil },
		depWithImage("spicebox-operator", "operator", pinned))

	assert.Contains(t, out, "spicebox-operator")
	assert.Contains(t, out, "5f4cd9990fe1", "short deployed digest")
	assert.Contains(t, out, "59bc10dc9219", "short current digest")
	assert.Contains(t, out, "--repair", "must point the user at the fix")
	assert.Equal(t, pinned, deployedImage(t, cs, "spicebox-operator"), "no repair → image unchanged")
}

func TestCheckImageDrift_Drift_RepairRepinsToCurrentDigest(t *testing.T) {
	pinned := operatorTagRef + "@" + testDeployed
	_, cs := runDrift(t, true,
		func(string) (string, error) { return testCurrent, nil },
		depWithImage("spicebox-operator", "operator", pinned))

	assert.Equal(t, operatorTagRef+"@"+testCurrent, deployedImage(t, cs, "spicebox-operator"),
		"repair must re-pin the container image to the current registry digest")
}

func TestCheckImageDrift_ResolveError_SoftWarnsNoFailNoRepin(t *testing.T) {
	pinned := operatorTagRef + "@" + testDeployed
	out, cs := runDrift(t, true,
		func(string) (string, error) { return "", errors.New("unauthenticated") },
		depWithImage("spicebox-operator", "operator", pinned))

	assert.Contains(t, out, "could not resolve")
	assert.Equal(t, pinned, deployedImage(t, cs, "spicebox-operator"), "resolve error → image unchanged")
}

// TestCheckImageDrift_MalformedRegistryDigest_SkipsAndNeverRepins covers the
// resolver returning a non-error, non-digest value. imagetoolsDigest parses
// buildx's `{{json .Manifest}}` output, and parseImagetoolsManifestDigest
// returns ("", nil) for any well-formed JSON object without a top-level
// "digest" field — so a buildx output-shape drift (which has bitten this
// codebase before) yields an empty digest with no error. That empty value can
// never equal the deployed digest, so every deployment reads as drifted, and
// under --repair the container image is patched to "<tagRef>@" — a malformed
// reference the apiserver accepts and the kubelet then cannot pull. Validate
// the digest before comparing, exactly as resolveMissingRegistryDigests does.
func TestCheckImageDrift_MalformedRegistryDigest_SkipsAndNeverRepins(t *testing.T) {
	pinned := operatorTagRef + "@" + testDeployed
	out, cs := runDrift(t, true,
		func(string) (string, error) { return "", nil },
		depWithImage("spicebox-operator", "operator", pinned))

	assert.Contains(t, out, "unexpected digest", "must name the malformed value, not report drift")
	assert.NotContains(t, out, "--repair", "an unresolvable digest is not drift; don't advertise the repair path")
	assert.Equal(t, pinned, deployedImage(t, cs, "spicebox-operator"),
		"a malformed digest must never re-pin the Deployment")
}

func TestCheckImageDrift_NonPinnedImage_Skipped(t *testing.T) {
	called := false
	out, _ := runDrift(t, true,
		func(string) (string, error) { called = true; return testCurrent, nil },
		depWithImage("spicebox-postgres", "postgres", "pgvector/pgvector:pg17"))

	assert.False(t, called, "must not resolve registry digest for a non-pinned image")
	assert.NotContains(t, out, "spicebox-postgres", "non-pinned image is silently skipped")
}

func TestParsePinnedRef(t *testing.T) {
	cases := []struct {
		name       string
		ref        string
		wantTagRef string
		wantDigest string
		wantReg    string
		wantOK     bool
	}{
		{
			name:       "digest-pinned registry ref: split into tagRef+digest+reg",
			ref:        "registry.example.com/ap/spicebox-operator:dev@sha256:5f4cd999",
			wantTagRef: "registry.example.com/ap/spicebox-operator:dev",
			wantDigest: "sha256:5f4cd999",
			wantReg:    "registry.example.com/ap",
			wantOK:     true,
		},
		{
			name:   "bare local :dev (no digest): not pinned",
			ref:    "spicebox-operator:dev",
			wantOK: false,
		},
		{
			name:   "public dependency tag (no digest): not pinned",
			ref:    "authzed/spicedb:latest",
			wantOK: false,
		},
		{
			name:   "registry ref with no digest: not pinned",
			ref:    "registry.example.com/ap/spicebox-operator:dev",
			wantOK: false,
		},
		{
			name:   "empty: not pinned",
			ref:    "",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tagRef, digest, reg, ok := parsePinnedRef(tc.ref)
			assert.Equal(t, tc.wantOK, ok, "ok")
			if !tc.wantOK {
				return
			}
			assert.Equal(t, tc.wantTagRef, tagRef, "tagRef")
			assert.Equal(t, tc.wantDigest, digest, "digest")
			assert.Equal(t, tc.wantReg, reg, "reg")
		})
	}
}

func TestRegistryRootOf(t *testing.T) {
	cases := []struct {
		name     string
		ref      string
		wantRoot string
		wantOK   bool
	}{
		{
			name:     "digest-pinned AR ref: extracts registry root",
			ref:      "us-east1-docker.pkg.dev/my-proj/ap/spicebox-operator:v1@sha256:abc",
			wantRoot: "us-east1-docker.pkg.dev/my-proj/ap",
			wantOK:   true,
		},
		{
			name:     "unpinned AR ref (--no-digest-pin): extracts registry root",
			ref:      "us-east1-docker.pkg.dev/my-proj/ap/spicebox-operator:v1",
			wantRoot: "us-east1-docker.pkg.dev/my-proj/ap",
			wantOK:   true,
		},
		{
			name:     "registry with an explicit port: extracts registry root",
			ref:      "localhost:5000/ap/spicebox-operator:dev",
			wantRoot: "localhost:5000/ap",
			wantOK:   true,
		},
		{
			name:   "bare local tag names no registry",
			ref:    "spicebox-operator:dev",
			wantOK: false,
		},
		{
			name:   "docker hub org path is not a registry we can push to",
			ref:    "acme-infra/spicedb:latest",
			wantOK: false,
		},
		{
			name:   "empty ref: rejected",
			ref:    "",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, ok := imagemode.RegistryRootOf(tc.ref)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantRoot, root)
		})
	}
}
