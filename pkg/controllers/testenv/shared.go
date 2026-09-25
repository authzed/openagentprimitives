package testenv

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// shared holds the one envtest cluster booted per package (per test binary /
// process). nil when envtest binaries are unavailable — Shared then skips.
var shared *Env

// RunPackage boots ONE envtest cluster for the whole package, runs all the
// package's tests against it, then stops it — so the apiserver+etcd are booted
// once per package instead of once per test. Use from TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(testenv.RunPackage(m)) }
//
// If envtest binaries are unavailable, it leaves `shared` nil and still runs
// the tests (each Shared(t) call then skips) — matching Start's skip behavior.
//
// Any OTHER boot failure aborts the package with a non-zero exit instead. This
// is the difference between "this machine cannot run envtest" and "envtest is
// broken": the second used to leave `shared` nil too, so every test in the
// package skipped and the suite exited 0 having verified nothing.
func RunPackage(m *testing.M) int {
	env, err := startShared()
	switch {
	case errors.Is(err, errAssetsUnavailable):
		fmt.Fprintf(os.Stderr, "testenv: %v — tests will skip\n", err)
	case err != nil:
		fmt.Fprintf(os.Stderr, "testenv: FATAL %v\n", err)
		return 1
	default:
		shared = env
		fmt.Fprintln(os.Stderr, "testenv: shared apiserver booted")
	}
	code := m.Run()
	if shared != nil {
		if err := shared.testEnv.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "testenv: stop shared apiserver: %v\n", err)
		}
		shared = nil
	}
	return code
}

// Shared returns the package-shared env, skipping the test if envtest is
// unavailable. It registers a t.Cleanup that resets cluster state after the
// test, so the next test sees an empty cluster.
func Shared(t *testing.T) *Env {
	t.Helper()
	if shared == nil {
		t.Skip("envtest unavailable (set KUBEBUILDER_ASSETS or run `mage test:integration`)")
	}
	t.Cleanup(func() { Reset(t, shared) })
	return shared
}

// Reset wipes the shared cluster back to empty between serially-run tests:
// it strips finalizers (tests drive Reconcile by hand, so the controller's
// finalizer-removal loop never runs and a plain delete would hang in
// Terminating), then deletes every spicebox CR kind (enumerated from the
// scheme, so new CRDs are covered automatically) plus the core kinds tests and
// the operator create.
//
// Reset intentionally does NOT delete namespaces — envtest has no namespace
// GC, so a namespace deletion hangs in Terminating; it empties each namespace
// instead. Tests that create a namespace must tolerate it already existing on a
// later test run within the same package.
func Reset(t *testing.T, env *Env) {
	t.Helper()
	ctx := context.Background()

	stripFinalizer := []byte(`{"metadata":{"finalizers":null}}`)

	// 1. spicebox CRs: list across all namespaces, strip finalizers, then delete
	// each item individually. DeleteAllOf on unstructured is not reliable in
	// envtest (no cluster-wide delete endpoint for namespaced resources).
	for _, gvk := range spiceboxKinds(env.Scheme) {
		ul := &unstructured.UnstructuredList{}
		ul.SetGroupVersionKind(schema.GroupVersionKind{Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind + "List"})
		if err := env.Client.List(ctx, ul); err != nil {
			t.Logf("testenv.Reset: list %s: %v", gvk.Kind, err)
			continue
		}
		for i := range ul.Items {
			it := &ul.Items[i]
			if len(it.GetFinalizers()) > 0 {
				if err := env.Client.Patch(ctx, it, client.RawPatch(types.MergePatchType, stripFinalizer)); err != nil {
					t.Logf("testenv.Reset: strip finalizer %s/%s: %v", it.GetNamespace(), it.GetName(), err)
				}
			}
			if err := env.Client.Delete(ctx, it); client.IgnoreNotFound(err) != nil {
				t.Logf("testenv.Reset: delete %s %s/%s: %v", gvk.Kind, it.GetNamespace(), it.GetName(), err)
			}
		}
	}

	// 2. core kinds: sweep every namespace (integration tests place objects in
	// "ns", "team-a", "agentprimitives-system", etc., not just "default"), so
	// the next test that shares this envtest sees an empty cluster. DeleteAllOf
	// needs InNamespace to route to the correct collection endpoint in envtest.
	// We do NOT delete the namespaces themselves — envtest has no namespace GC,
	// so a namespace delete hangs in Terminating; leaving them empty is correct.
	var nsList corev1.NamespaceList
	if err := env.Client.List(ctx, &nsList); err != nil {
		t.Logf("testenv.Reset: list namespaces: %v", err)
	}
	coreKinds := []client.Object{
		&corev1.Secret{}, &corev1.ConfigMap{}, &corev1.Pod{},
		&corev1.ServiceAccount{}, &corev1.PersistentVolumeClaim{},
		&rbacv1.Role{}, &rbacv1.RoleBinding{},
		&networkingv1.NetworkPolicy{},
	}
	for _, ns := range nsList.Items {
		switch ns.Name {
		case "kube-system", "kube-public", "kube-node-lease":
			continue
		}
		for _, obj := range coreKinds {
			if err := env.Client.DeleteAllOf(ctx, obj, client.InNamespace(ns.Name)); err != nil {
				t.Logf("testenv.Reset: deleteAllOf %T in %s: %v", obj, ns.Name, err)
			}
		}
		// Jobs need explicit Background propagation for DeleteAllOf: the Kubernetes
		// API server defaults to Orphan propagation for Jobs (adds an "orphan"
		// finalizer that the GC must clear), and the GC does not run in envtest.
		// With Background propagation the API server removes the object from etcd
		// immediately (no finalizer added, no GC needed) since there are no
		// in-envtest pods to cascade.
		if err := env.Client.DeleteAllOf(ctx, &batchv1.Job{},
			client.InNamespace(ns.Name),
			client.PropagationPolicy(metav1.DeletePropagationBackground),
		); err != nil {
			t.Logf("testenv.Reset: deleteAllOf Job in %s: %v", ns.Name, err)
		}
	}

	// Cluster-scoped ClusterRole/ClusterRoleBinding are intentionally NOT swept:
	// an unfiltered DeleteAllOf would also remove the apiserver's system:* RBAC
	// bindings, degrading RBAC enforcement for the rest of the shared envtest (it
	// broke the agentsession impersonated-SA enforcement test). The CRBs the
	// agentsession reconciler creates are UID-named and benign, and they vanish
	// with the apiserver when TestMain stops it — so nothing is left after the
	// suite.
}

// spiceboxKinds returns the singular GVKs of every spicebox CR registered in
// the scheme (group == v1alpha1.GroupName, version v1alpha1), excluding List
// kinds. Enumerating from the scheme means a new CRD is wiped automatically.
func spiceboxKinds(sch *apiruntime.Scheme) []schema.GroupVersionKind {
	var out []schema.GroupVersionKind
	for gvk := range sch.AllKnownTypes() {
		if gvk.Group != spiceboxv1alpha1.GroupName || gvk.Version != spiceboxv1alpha1.SchemeGroupVersion.Version {
			continue
		}
		// Skip the meta kinds metav1.AddToGroupVersion registers into every
		// group-version (*List, *Options, WatchEvent) — they are not CRDs, so
		// listing them in Reset returns a benign "no matches" the List-error
		// log would otherwise surface as noise on every test.
		if strings.HasSuffix(gvk.Kind, "List") || strings.HasSuffix(gvk.Kind, "Options") || gvk.Kind == "WatchEvent" {
			continue
		}
		out = append(out, gvk)
	}
	return out
}
