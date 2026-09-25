package imagemode

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/imageload"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"

	// The cases below name cluster kinds by key; oap registers every strategy
	// from main, so cloud.MustFor here needs the same set linked in.
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/aks"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/desktop"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/eks"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/gke"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/local"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/unmanaged"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

// nodeWithProviderID constructs a minimal Node fixture with the given name and
// spec.providerID, used by tests that exercise providerID-based cloud detection.
func nodeWithProviderID(name, providerID string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{ProviderID: providerID},
	}
}

func TestFirstNodeInfo(t *testing.T) {
	// zero nodes → found=false, no error (an empty cluster is not a List failure).
	_, _, found, err := FirstNodeInfo(context.Background(), fake.NewSimpleClientset())
	require.NoError(t, err)
	assert.False(t, found)

	// a node present → found=true, providerID + arch returned.
	n := nodeWithProviderID("n1", "gce://proj/us-central1-a/gke-x")
	n.Status.NodeInfo.Architecture = "amd64"
	arch, pid, found, err := FirstNodeInfo(context.Background(), fake.NewSimpleClientset(n))
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "gce://proj/us-central1-a/gke-x", pid)
	assert.Equal(t, "amd64", arch)
}

func TestResolveRemoteRegistryFromCluster_noNodesErrors(t *testing.T) {
	// A remote cluster with no nodes can't be type/registry-detected: fail closed
	// with an actionable error (not a suggestion-less prompt). in is nil because
	// the no-nodes path returns before reading stdin.
	_, err := ResolveRemoteFromCluster(
		context.Background(), fake.NewSimpleClientset(), cloud.MustFor(cloud.KeyGKE),
		"gke_example-project_us-east1_ap-zap-internal", io.Discard, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no nodes")
	assert.Contains(t, err.Error(), "--image-registry")
}

// A cluster with an operator already installed must not prompt: the existing
// adapter signature keeps init/install call sites unchanged while gaining the
// operator-image inference.
func TestResolveRemoteRegistryFromClusterPrefersInstalledOperator(t *testing.T) {
	const installed = "europe-west4-docker.pkg.dev/acme-infra/ap"
	kc := fake.NewSimpleClientset(
		aptest.OperatorDeployment(installed+"/spicebox-operator:v1@sha256:abc"),
		aptest.NodeWithProviderID("gce://my-proj/us-east1-c/n"),
	)
	got, err := ResolveRemoteFromCluster(context.Background(), kc, gkeDeriver{"us-east1-docker.pkg.dev/my-proj/ap"}, "test-ctx", io.Discard, nil)
	require.NoError(t, err)
	assert.Equal(t, installed, got, "must use the cluster's own registry without prompting")
}

func TestResolveImageMode(t *testing.T) {
	cases := []struct {
		name         string
		reg          string
		profile      cloud.InstallProfile
		kctx         string
		wantRegistry string
		wantLocal    bool
		wantRemote   bool
	}{
		{"explicit registry wins", "myreg.io/ap", cloud.ManagedProfile, "remote", "myreg.io/ap", false, false},
		{"local profile forces local images", "", cloud.DevProfile, "remote", "", true, false},
		{"kind context → local load", "", cloud.ManagedProfile, "kind-ap", "", true, false},
		{"empty kctx + unmanaged → local (undetectable cluster)", "", cloud.ProductionProfile, "", "", true, false},
		{"managed + no registry → needsRemote", "", cloud.ManagedProfile, "eks-ctx", "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg, useLocal, needsRemote, err := Resolve(tc.reg, tc.profile, tc.kctx)
			require.NoError(t, err)
			assert.Equal(t, tc.wantRegistry, reg)
			assert.Equal(t, tc.wantLocal, useLocal)
			assert.Equal(t, tc.wantRemote, needsRemote)
		})
	}
}

// Resolve's notion of "local" must be exactly imageload's — a context
// that needs no registry is local, and nothing else is. This asserts the two
// stayed collapsed after isLocalClusterContext was deleted.
func TestResolveImageModeLocalMatchesImageloadClassification(t *testing.T) {
	cases := []struct {
		name      string
		context   string
		wantLocal bool
	}{
		{"kind: local", "kind-mycluster", true},
		{"k3d: local", "k3d-mycluster", true},
		{"minikube: local", "minikube", true},
		{"docker-desktop: local", "docker-desktop", true},
		{"oap-desktop: local (VM load, no registry needed)", imageload.APContextName, true},
		{"GKE: not local", "gke_my-proj_us-east1_prod", false},
		{"unrecognized: not local", "my-prod-eks", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// cloud.ManagedProfile stands in so the kctx=="" fallback in
			// Resolve is not what decides these cases.
			_, useLocal, needsRemote, err := Resolve("", cloud.ManagedProfile, tc.context)
			require.NoError(t, err)
			assert.Equal(t, tc.wantLocal, useLocal)
			assert.Equal(t, !tc.wantLocal, needsRemote)
		})
	}
}

func TestResolveRemoteRegistry(t *testing.T) {
	// non-interactive → error (suggestion named if present)
	_, err := resolveRemoteRegistry("sugg.io/ap", false, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sugg.io/ap")
	_, err = resolveRemoteRegistry("", false, nil)
	require.Error(t, err)

	// interactive: prompt returns the typed value (default prefilled by prompt impl)
	got, err := resolveRemoteRegistry("sugg.io/ap", true, func(label, def string) (string, error) {
		assert.Equal(t, "sugg.io/ap", def)
		return "typed.io/ap", nil
	})
	require.NoError(t, err)
	assert.Equal(t, "typed.io/ap", got)

	// interactive but empty answer → error
	_, err = resolveRemoteRegistry("", true, func(label, def string) (string, error) { return "", nil })
	require.Error(t, err)
}

func TestDeriveRegistry(t *testing.T) {
	cases := []struct {
		name, cloudProviderID, want string
		strat                       cloud.Strategy
	}{
		{"gke → artifact registry", "gce://my-proj/us-central1-a/gke-node-1", "us-central1-docker.pkg.dev/my-proj/ap", cloud.MustFor(cloud.KeyGKE)},
		{"gke multi-segment region", "gce://p/europe-west4-b/n", "europe-west4-docker.pkg.dev/p/ap", cloud.MustFor(cloud.KeyGKE)},
		{"eks → no derivation (account unknown)", "aws:///us-west-2a/i-0abc", "", cloud.MustFor(cloud.KeyEKS)},
		{"aks → no derivation", "azure:///subscriptions/s/resourceGroups/rg/x", "", cloud.MustFor(cloud.KeyAKS)},
		{"unknown → none", "", "", cloud.MustFor(cloud.KeyDefault)},
		{"malformed gke → none", "gce://onlyproj", "", cloud.MustFor(cloud.KeyGKE)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, deriveRegistry(tc.strat, tc.cloudProviderID))
		})
	}
}

func TestChannelsDGroupRegistrySubstituted(t *testing.T) {
	ch, err := manifestsChannelsDForTest()
	require.NoError(t, err)
	found := false
	for _, doc := range ch {
		out, serr := manifestsSubstituteForTest(doc, "myreg.io/ap")
		require.NoError(t, serr)
		if bytesContains(out, "myreg.io/ap/agentprimitives-channelsd:dev") {
			found = true
		}
	}
	assert.True(t, found, "the channelsd manifest group must have its image registry-substituted")
}

func manifestsChannelsDForTest() ([][]byte, error) { return manifests.ChannelsD() }
func manifestsSubstituteForTest(in []byte, reg string) ([]byte, error) {
	return manifests.Substitute(in, manifests.Tags{Registry: reg})
}
func bytesContains(b []byte, s string) bool { return strings.Contains(string(b), s) }

// gkeDeriver derives an Artifact Registry root the way pkg/platform/cloud/gke does,
// without pulling the real strategy (and its cloud deps) into this test.
type gkeDeriver struct{ out string }

func (g gkeDeriver) RegistryFromProviderID(string) string { return g.out }

func TestResolveRegistryForCluster(t *testing.T) {
	const derived = "us-east1-docker.pkg.dev/my-proj/ap"
	const installed = "europe-west4-docker.pkg.dev/acme-infra/ap"

	cases := []struct {
		name        string
		flag        string
		objs        []runtime.Object
		deriver     gkeDeriver
		interactive bool
		promptAns   string
		want        string
		wantErr     string
	}{
		{
			name:    "explicit flag wins over everything",
			flag:    "myreg.io/ap",
			objs:    []runtime.Object{aptest.OperatorDeployment(installed + "/spicebox-operator:v1"), aptest.NodeWithProviderID("gce://my-proj/us-east1-c/n")},
			deriver: gkeDeriver{derived},
			want:    "myreg.io/ap",
		},
		{
			name:    "installed operator image wins over the cloud derivation, no prompt",
			objs:    []runtime.Object{aptest.OperatorDeployment(installed + "/spicebox-operator:v1@sha256:abc"), aptest.NodeWithProviderID("gce://my-proj/us-east1-c/n")},
			deriver: gkeDeriver{derived},
			want:    installed,
		},
		{
			name:    "unpinned operator image still yields its registry",
			objs:    []runtime.Object{aptest.OperatorDeployment(installed + "/spicebox-operator:v1"), aptest.NodeWithProviderID("gce://my-proj/us-east1-c/n")},
			deriver: gkeDeriver{derived},
			want:    installed,
		},
		{
			name:        "no operator installed: falls through to the cloud derivation",
			objs:        []runtime.Object{aptest.NodeWithProviderID("gce://my-proj/us-east1-c/n")},
			deriver:     gkeDeriver{derived},
			interactive: true,
			promptAns:   "", // Enter accepts the prefilled suggestion
			want:        derived,
		},
		{
			name:    "bare :dev operator image names no registry: falls through",
			objs:    []runtime.Object{aptest.OperatorDeployment("spicebox-operator:dev"), aptest.NodeWithProviderID("gce://my-proj/us-east1-c/n")},
			deriver: gkeDeriver{derived},
			wantErr: "suggested: " + derived,
		},
		{
			name:    "non-interactive with a suggestion: fails closed, names the suggestion",
			objs:    []runtime.Object{aptest.NodeWithProviderID("gce://my-proj/us-east1-c/n")},
			deriver: gkeDeriver{derived},
			wantErr: "suggested: " + derived,
		},
		{
			name:    "no nodes: fails closed, cannot detect cloud or registry",
			objs:    nil,
			deriver: gkeDeriver{derived},
			wantErr: "has no nodes",
		},
		{
			name:    "non-derivable cloud, non-interactive: fails closed with no suggestion",
			objs:    []runtime.Object{aptest.NodeWithProviderID("aws:///us-west-2a/i-0abc")},
			deriver: gkeDeriver{""},
			wantErr: "needs an image registry",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kc := fake.NewSimpleClientset(tc.objs...)
			prompt := func(_, def string) (string, error) {
				if tc.promptAns == "" {
					return def, nil
				}
				return tc.promptAns, nil
			}
			got, err := ResolveForCluster(context.Background(), kc, tc.deriver,
				"test-ctx", tc.flag, io.Discard, prompt, tc.interactive)
			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestResolveRegistryForCluster_operatorReadError covers the one branch the
// table above cannot reach: a real API failure reading the operator
// Deployment (as opposed to NotFound, which every other case either produces
// or avoids by supplying a Deployment that succeeds). The contract is
// fail-OPEN, not fail-silent: resolution must fall through to the
// cloud-derived suggestion rather than abort, AND the failure must be
// surfaced on out rather than swallowed.
func TestResolveRegistryForCluster_operatorReadError(t *testing.T) {
	const derived = "us-east1-docker.pkg.dev/my-proj/ap"

	kc := fake.NewSimpleClientset(aptest.NodeWithProviderID("gce://my-proj/us-east1-c/n"))
	kc.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("dial tcp 127.0.0.1:6443: connect: connection refused")
	})

	var out bytes.Buffer
	prompt := func(_, def string) (string, error) { return def, nil } // Enter accepts the prefilled suggestion
	got, err := ResolveForCluster(context.Background(), kc, gkeDeriver{derived},
		"test-ctx", "" /* no flag */, &out, prompt, true /* interactive */)

	require.NoError(t, err, "an unreadable operator Deployment must fall through, not abort")
	assert.Equal(t, derived, got, "falls through to the cloud-derived suggestion")
	assert.Contains(t, out.String(), "could not infer the image registry from the installed operator",
		"the read failure must be surfaced on out, not swallowed")
}
