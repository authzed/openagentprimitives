package installcmd

import (
	"bytes"
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	k8stesting "k8s.io/client-go/testing"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

func TestCleanRequiresYesWhenNonTTY(t *testing.T) {
	root := newRoot(t)
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	// stdin not a TTY in tests; no -y; should error.
	root.SetArgs([]string{"clean"})
	err := root.Execute()
	require.Error(t, err, "clean without -y in non-TTY should error")
	assert.Contains(t, err.Error(), "--yes", "error should mention --yes")
}

func TestCleanOrphanFinalizersFlagInUsage(t *testing.T) {
	// We can't easily exercise the actual orphan-patch path without a live
	// cluster fixture, but we can at least confirm the flag is registered
	// and that --help mentions it.
	root := newRoot(t)
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetArgs([]string{"clean", "--help"})
	require.NoError(t, root.Execute(), "clean --help")
	assert.Contains(t, stdout.String(), "orphan-finalizers", "--orphan-finalizers should appear in clean --help")
}

func TestCleanWithYesProceeds(t *testing.T) {
	root := newRoot(t)
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetArgs([]string{"clean", "--yes", "--dry-run=client"})
	// In dry-run mode the kube client isn't built; the command prints the
	// intended deletion plan and exits 0.
	require.NoError(t, root.Execute(), "clean -y --dry-run=client should succeed")
	out := strings.ToLower(stdout.String())
	for _, want := range []string{"would delete", "agentidentities", "agentclasses", "agentsessions", "namespace agentprimitives-system", "ap-workspace-rwx", "namespace ap-workspace-storage", "would keep the artifact store bucket"} {
		assert.Containsf(t, out, want, "dry-run output missing %q", want)
	}
	assert.NotContains(t, out, "would delete the artifact store bucket", "default dry-run must not plan an artifact-store delete")
}

// TestCleanDryRunDeleteArtifactsFlagsBucketDeletion verifies --delete-artifacts
// flips the dry-run plan line from "keep" to "DELETE" — the flag's only
// user-visible effect under --dry-run=client, since no kube client is built.
func TestCleanDryRunDeleteArtifactsFlagsBucketDeletion(t *testing.T) {
	root := newRoot(t)
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetArgs([]string{"clean", "--yes", "--dry-run=client", "--delete-artifacts"})
	require.NoError(t, root.Execute(), "clean -y --dry-run=client --delete-artifacts should succeed")
	out := stdout.String()
	assert.Contains(t, out, "would DELETE the artifact store bucket (--delete-artifacts; label-guarded)")
	assert.NotContains(t, out, "would keep the artifact store bucket")
}

// TestRemoveArtifactStore covers removeArtifactStore's own branches directly
// (empty URL, file://, mem://, and a real Teardown round-trip) without a live
// cluster: an empty fake typed clientset has no Nodes, so cloud.Detect falls
// back to the registered default (local.Strategy), whose ArtifactStorage()
// is cloud.RequireExplicitArtifactStorage — deterministic, no cloud APIs hit.
func TestRemoveArtifactStore(t *testing.T) {
	newBundle := func() *kube.Bundle {
		return &kube.Bundle{Typed: k8sfake.NewSimpleClientset()}
	}

	cases := []struct {
		name            string
		url             string
		deleteArtifacts bool
		wantContains    []string
		wantEmpty       bool
	}{
		{name: "empty URL, keep: silent (nothing was ever recorded, nothing to warn about)", url: "", deleteArtifacts: false, wantEmpty: true},
		{name: "empty URL, --delete-artifacts: warns nothing to delete", url: "", deleteArtifacts: true, wantContains: []string{"no artifact store URL recorded"}},
		{name: "file:// URL: no-op, torn down with the namespace", url: "file:///data/artifacts", deleteArtifacts: true, wantEmpty: true},
		{name: "mem:// URL: no-op, in-process store", url: "mem://", deleteArtifacts: true, wantEmpty: true},
		{name: "cloud URL, keep: RequireExplicitArtifactStorage reports KeptMessage", url: "gs://ap-artifacts-p-abcd1234", deleteArtifacts: false, wantContains: []string{"was not created by oap", "left untouched"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			removeArtifactStore(context.Background(), &out, newBundle(), tc.url, tc.deleteArtifacts)
			if tc.wantEmpty {
				assert.Empty(t, out.String())
				return
			}
			for _, want := range tc.wantContains {
				assert.Contains(t, out.String(), want)
			}
		})
	}
}

func TestRemoveAPInstalledComponentsGatesByAnnotation(t *testing.T) {
	certNS := func(annotated bool) *corev1.Namespace {
		n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cert-manager"}}
		if annotated {
			n.Annotations = map[string]string{kube.InstalledByAnnotation: kube.InstalledByValue}
		}
		return n
	}
	bundle := func(ns *corev1.Namespace) *kube.Bundle {
		return &kube.Bundle{
			Typed:   k8sfake.NewSimpleClientset(ns),
			Dynamic: dynfake.NewSimpleDynamicClient(scheme.Scheme),
		}
	}

	// Pre-existing (unmarked) cert-manager → never touched (the safety guarantee).
	var leave bytes.Buffer
	removeAPInstalledComponents(context.Background(), &leave, bundle(certNS(false)))
	assert.Contains(t, leave.String(), "not installed by oap", "an unmarked component must be left alone")
	assert.NotContains(t, leave.String(), "removing cert-manager")

	// oap-installed (annotated) cert-manager → removal proceeds.
	var remove bytes.Buffer
	removeAPInstalledComponents(context.Background(), &remove, bundle(certNS(true)))
	assert.Contains(t, remove.String(), "removing cert-manager", "an oap-installed component must be removed")
}

// TestRemoveWorkspaceStorage verifies the bundled workspace-storage provisioner
// tier (the oap-workspace-storage namespace + cluster-scoped StorageClass/RBAC)
// is fully torn down — the gap that left ap-workspace-provisioner Running after
// `oap clean`. Reactors record every delete the helper issues, sidestepping the
// dynamic fake's flaky cluster-scoped-resource tracking.
func TestRemoveWorkspaceStorage(t *testing.T) {
	type del struct{ resource, ns, name string }

	// want is every object the tier installs; the cluster-scoped StorageClass +
	// RBAC (ns "") are the ones a namespace delete alone would orphan.
	want := []del{
		{"storageclasses", "", "ap-workspace-rwx"},
		{"clusterroles", "", "ap-workspace-provisioner"},
		{"clusterrolebindings", "", "ap-workspace-provisioner"},
		{"deployments", "ap-workspace-storage", "ap-workspace-provisioner"},
		{"serviceaccounts", "ap-workspace-storage", "ap-workspace-provisioner"},
		{"configmaps", "ap-workspace-storage", "ap-workspace-provisioner-config"},
		{"configmaps", "agentprimitives-system", "ap-workspace-config"},
	}

	newBundle := func(t *testing.T) (b *kube.Bundle, objDeletes *[]del, nsDeletes *[]string) {
		t.Helper()
		objDeletes, nsDeletes = &[]del{}, &[]string{}

		dyn := dynfake.NewSimpleDynamicClient(scheme.Scheme)
		dyn.PrependReactor("delete", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
			da := a.(k8stesting.DeleteAction)
			*objDeletes = append(*objDeletes, del{a.GetResource().Resource, a.GetNamespace(), da.GetName()})
			return true, nil, nil // simulate a successful delete; skip the tracker
		})

		typed := k8sfake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ap-workspace-storage"}})
		typed.PrependReactor("delete", "namespaces", func(a k8stesting.Action) (bool, runtime.Object, error) {
			*nsDeletes = append(*nsDeletes, a.(k8stesting.DeleteAction).GetName())
			return true, nil, nil
		})

		return &kube.Bundle{Typed: typed, Dynamic: dyn}, objDeletes, nsDeletes
	}

	t.Run("keepNs=false: deletes every workspace object AND the namespace", func(t *testing.T) {
		b, objDeletes, nsDeletes := newBundle(t)
		var out bytes.Buffer
		removeWorkspaceStorage(context.Background(), &out, b, false)
		assert.ElementsMatch(t, want, *objDeletes, "every workspace object (incl. cluster-scoped) must be deleted")
		assert.Equal(t, []string{"ap-workspace-storage"}, *nsDeletes, "the workspace namespace must be deleted")
	})

	t.Run("keepNs=true: deletes the workload but preserves the namespace", func(t *testing.T) {
		b, objDeletes, nsDeletes := newBundle(t)
		var out bytes.Buffer
		removeWorkspaceStorage(context.Background(), &out, b, true)
		assert.ElementsMatch(t, want, *objDeletes, "objects are still removed under --keep-namespace")
		assert.Empty(t, *nsDeletes, "the namespace must be preserved under --keep-namespace")
	})
}

// TestOperatorArtifactStoreURL verifies clean reads ARTIFACT_STORE_URL off
// the live operator Deployment — the single source of truth for which store
// an install used — and returns "" (never an error) when the Deployment is
// absent, so clean can proceed on a half-torn cluster.
func TestOperatorArtifactStoreURL(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "spicebox-operator", Namespace: "agentprimitives-system"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "operator",
				Env:  []corev1.EnvVar{{Name: "ARTIFACT_STORE_URL", Value: "gs://ap-artifacts-p-abcd1234"}},
			}},
		}}},
	}
	b := &kube.Bundle{Typed: k8sfake.NewSimpleClientset(dep)}
	assert.Equal(t, "gs://ap-artifacts-p-abcd1234", operatorArtifactStoreURL(context.Background(), b))

	empty := &kube.Bundle{Typed: k8sfake.NewSimpleClientset()}
	assert.Equal(t, "", operatorArtifactStoreURL(context.Background(), empty), "absent Deployment ⇒ empty, no error surface needed")
}

func TestCleanCoversEveryCRD(t *testing.T) {
	matches, err := filepath.Glob("../../../../config/crds/agentprimitives.authzed.com_*.yaml")
	require.NoError(t, err, "glob CRD YAMLs")
	require.NotEmpty(t, matches, "no CRD YAMLs found — test misconfigured?")

	expected := map[string]bool{}
	const prefix = "agentprimitives.authzed.com_"
	const suffix = ".yaml"
	for _, m := range matches {
		name := filepath.Base(m)
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
			continue
		}
		expected[name[len(prefix):len(name)-len(suffix)]] = true
	}

	actual := map[string]bool{}
	for _, gvr := range crGroupVersionResources {
		if gvr.Group == "agentprimitives.authzed.com" {
			actual[gvr.Resource] = true
		}
	}

	var missing, extra []string
	for r := range expected {
		if !actual[r] {
			missing = append(missing, r)
		}
	}
	for r := range actual {
		if !expected[r] {
			extra = append(extra, r)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	assert.Empty(t, missing, "crGroupVersionResources missing CRDs")
	assert.Empty(t, extra, "crGroupVersionResources contains entries with no CRD")
}

func TestUnwedgeNamespaceIfStuck_PrintsReport(t *testing.T) {
	// A fake strategy returns a populated report; the helper must print the
	// stuck finalizer, the manual commands, and what it cleared.
	rep := cloud.UnwedgeReport{
		StuckFinalizers:   []string{"networking.gke.io/neg-finalizer"},
		FinalizersCleared: []string{"neg-obj"},
		ManualCommands:    []string{"gcloud compute network-endpoint-groups delete x --zone us-east1-b --project acme-proj --quiet"},
	}
	prev := unwedgeStrategyForTest
	unwedgeStrategyForTest = fakeUnwedgeStrategy{report: rep}
	t.Cleanup(func() { unwedgeStrategyForTest = prev })

	var out bytes.Buffer
	b := &kube.Bundle{
		Typed:   k8sfake.NewSimpleClientset(),
		Dynamic: dynfake.NewSimpleDynamicClient(scheme.Scheme),
	}
	unwedgeNamespaceIfStuck(context.Background(), &out, b, "agentprimitives-system", true)

	s := out.String()
	assert.Contains(t, s, "networking.gke.io/neg-finalizer")
	assert.Contains(t, s, "cleared")
}

// TestUnwedgeNamespaceIfStuck_ManualCommandsSurface verifies that when
// remediate=false (default oap clean, no --orphan-finalizers), the helper prints
// the manual fix commands AND the --orphan-finalizers re-run hint.
func TestUnwedgeNamespaceIfStuck_ManualCommandsSurface(t *testing.T) {
	const manualCmd = "gcloud compute network-endpoint-groups delete x --zone us-east1-b --project acme-proj --quiet"
	rep := cloud.UnwedgeReport{
		StuckFinalizers: []string{"networking.gke.io/neg-finalizer"},
		ManualCommands:  []string{manualCmd},
	}
	prev := unwedgeStrategyForTest
	unwedgeStrategyForTest = fakeUnwedgeStrategy{report: rep}
	t.Cleanup(func() { unwedgeStrategyForTest = prev })

	var out bytes.Buffer
	b := &kube.Bundle{
		Typed:   k8sfake.NewSimpleClientset(),
		Dynamic: dynfake.NewSimpleDynamicClient(scheme.Scheme),
	}
	unwedgeNamespaceIfStuck(context.Background(), &out, b, "agentprimitives-system", false)

	s := out.String()
	assert.Contains(t, s, manualCmd, "output must contain the manual GCP command")
	assert.Contains(t, s, "--orphan-finalizers", "output must hint at re-running with --orphan-finalizers")
}

type fakeUnwedgeStrategy struct {
	cloud.Strategy
	report cloud.UnwedgeReport
}

func (f fakeUnwedgeStrategy) UnwedgeTerminatingNamespace(context.Context, cloud.Clients, cloud.Reporter, string, bool) (cloud.UnwedgeReport, error) {
	return f.report, nil
}

func TestRemoveStatefulStorage_DeletesLabeledClass(t *testing.T) {
	gvrSC := schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}
	labeled := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "storage.k8s.io/v1",
		"kind":       "StorageClass",
		"metadata": map[string]any{
			"name":   "ap-stateful-hyperdisk",
			"labels": map[string]any{"agentprimitives.authzed.com/install-tier": "stateful-storage"},
		},
	}}
	other := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "storage.k8s.io/v1",
		"kind":       "StorageClass",
		"metadata":   map[string]any{"name": "standard-rwo"},
	}}
	dyn := dynfake.NewSimpleDynamicClient(scheme.Scheme, labeled, other)
	b := &kube.Bundle{Dynamic: dyn}
	var out bytes.Buffer

	removeStatefulStorage(context.Background(), &out, b)

	_, err := dyn.Resource(gvrSC).Get(context.Background(), "ap-stateful-hyperdisk", metav1.GetOptions{})
	assert.True(t, errors.IsNotFound(err), "labeled class should be deleted")
	_, err = dyn.Resource(gvrSC).Get(context.Background(), "standard-rwo", metav1.GetOptions{})
	require.NoError(t, err, "unlabeled class must be left alone")
}
