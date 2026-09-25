package install

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
)

// managedKind pairs a Kind Uninstall walks with whether it is cluster-scoped
// (list cluster-wide, no namespace) or namespaced (list scoped to a
// namespace, when one is given).
type managedKind struct {
	gvk           schema.GroupVersionKind
	clusterScoped bool
}

// managedKinds enumerates every Kind an oap Install can leave on the cluster,
// so Uninstall reaps exactly what Install could have created — no more, no
// less. It mirrors two things kept in sync by hand:
//
//   - The agentprimitives.authzed.com CRD kinds a .oap bundle can carry
//     (oap.allowedBundleKinds), plus the second-level SpiceboxToolkit the
//     export walk follows and Channel resources created for a composed graph.
//   - The two core Kinds Install itself can write: Secret (materialized from
//     SecretSpecs) and ConfigMap (a bundle can carry one, e.g. an inline
//     system-prompt or sidecar-script source).
//
// TestManagedKindsCoverAllowedBundleKinds enforces the first bullet, so the
// next kind added to the bundle allowlist fails a test here instead of silently
// orphaning on uninstall the way AgentUI once did.
//
// Cluster scope mirrors each type's +kubebuilder:resource:scope marker, and
// matches apply.go's clusterScopedKinds for the three CRD kinds a .oap can
// carry. Skill, SkillSource, and AgentUI are Namespaced.
var managedKinds = []managedKind{
	{gvk: v1alpha1.SchemeGroupVersion.WithKind("AgentClass")},
	{gvk: v1alpha1.SchemeGroupVersion.WithKind("AgentIdentity")},
	{gvk: v1alpha1.SchemeGroupVersion.WithKind("AgentUI")},
	{gvk: v1alpha1.SchemeGroupVersion.WithKind("Channel")},
	{gvk: v1alpha1.SchemeGroupVersion.WithKind("MCPServer")},
	{gvk: v1alpha1.SchemeGroupVersion.WithKind("SidecarToolbox")},
	{gvk: v1alpha1.SchemeGroupVersion.WithKind("Skill")},
	{gvk: v1alpha1.SchemeGroupVersion.WithKind("SkillSource")},
	{gvk: v1alpha1.SchemeGroupVersion.WithKind("SpiceboxClass"), clusterScoped: true},
	{gvk: v1alpha1.SchemeGroupVersion.WithKind("SpiceboxToolspec"), clusterScoped: true},
	{gvk: v1alpha1.SchemeGroupVersion.WithKind("SpiceboxToolkit"), clusterScoped: true},
	{gvk: corev1.SchemeGroupVersion.WithKind("Secret")},
	{gvk: corev1.SchemeGroupVersion.WithKind("ConfigMap")},
}

// Uninstall deletes every object across managedKinds carrying
// instance.LabelInstall = name, and returns how many were deleted.
//
// Selection is by instance.LabelInstall ALONE, deliberately. Install's Stamp
// call is the only thing that ever sets it, and it stamps only the bundle's own
// CRs and the Secrets install materializes. A cluster-scoped dependency this
// install merely ADOPTED because it was already present and pin-compatible
// carries adoptguard.AdoptedLabel plus an ownership annotation instead, never
// this one — so a shared adopted dependency is never a deletion candidate here,
// even though EnsureClusterDeps touched its metadata during install.
//
// namespace scopes every namespaced Kind's List; empty means cluster-wide.
// Cluster-scoped Kinds are always listed cluster-wide, since a namespace is
// meaningless for them (and the apiserver rejects one on such a List).
//
// A List or Delete failure for one Kind/object does not abort the walk:
// Uninstall makes a best-effort pass, aggregating every failure via
// errors.Join, so one missing CRD or stuck finalizer doesn't strand the rest. A
// NotFound on Delete (the object vanished between List and Delete) is not an
// error — "gone" already holds — and still counts toward deleted. No object
// whose Delete returned any other error is counted.
func Uninstall(ctx context.Context, c client.Client, name, namespace string) (deleted int, err error) {
	return UninstallGraph(ctx, c, name, namespace)
}

// UninstallGraph discovers the complete private graph under its root label,
// deleting all root resources before parents and then deeper dependencies.
// Missing annotations retain legacy single-node uninstall semantics.
func UninstallGraph(ctx context.Context, c client.Client, rootName, namespace string) (deleted int, err error) {
	var errs []error
	var objects []unstructured.Unstructured
	for _, mk := range managedKinds {
		items, kerr := listManagedKind(ctx, c, mk, rootName, namespace)
		if kerr != nil {
			errs = append(errs, kerr)
		}
		objects = append(objects, items...)
	}
	slices.SortStableFunc(objects, func(a, b unstructured.Unstructured) int {
		ap, bp := a.GetAnnotations()[AnnotationDependencyPath], b.GetAnnotations()[AnnotationDependencyPath]
		if order := cmp.Compare(dependencyPathDepth(ap), dependencyPathDepth(bp)); order != 0 {
			return order
		}
		return cmp.Compare(ap, bp)
	})
	for i := range objects {
		item := &objects[i]
		uid, resourceVersion := item.GetUID(), item.GetResourceVersion()
		deleteOptions := []client.DeleteOption{}
		if uid != "" || resourceVersion != "" {
			deleteOptions = append(deleteOptions, client.Preconditions(metav1.Preconditions{UID: &uid, ResourceVersion: &resourceVersion}))
		}
		if err := c.Delete(ctx, item, deleteOptions...); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("uninstall %s: delete %s %s/%s: %w", displayGraphPath(rootName, annotationPath(item)), item.GetKind(), item.GetNamespace(), item.GetName(), err))
			continue
		}
		deleted++
	}
	return deleted, errors.Join(errs...)
}

func dependencyPathDepth(path string) int {
	if path == "" {
		return 0
	}
	return strings.Count(path, " > ") + 1
}

func annotationPath(obj *unstructured.Unstructured) []string {
	path := obj.GetAnnotations()[AnnotationDependencyPath]
	if path == "" {
		return nil
	}
	return strings.Split(path, " > ")
}

func listManagedKind(ctx context.Context, c client.Client, mk managedKind, name, namespace string) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: mk.gvk.Group, Version: mk.gvk.Version, Kind: mk.gvk.Kind + "List"})

	opts := []client.ListOption{client.MatchingLabels{instance.LabelInstall: name}}
	if !mk.clusterScoped && namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := c.List(ctx, list, opts...); err != nil {
		return nil, fmt.Errorf("uninstall %s: list %s: %w", name, mk.gvk.Kind, err)
	}
	if mk.clusterScoped && namespace != "" {
		// Match installation's namespace ownership guard. Legacy objects with
		// no namespace stamp retain their existing name-only ownership.
		list.Items = slices.DeleteFunc(list.Items, func(obj unstructured.Unstructured) bool {
			ownerNamespace := obj.GetLabels()[instance.LabelInstallNamespace]
			return ownerNamespace != "" && ownerNamespace != namespace
		})
	}
	return list.Items, nil
}
