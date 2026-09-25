package install

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/tools/adoptkit"
)

// clusterDepOwnerKind is the ownership-annotation kind recorded on a cluster
// dep adopted by an oap install, so a co-adopted shared resource's annotations
// distinguish "installed alongside AgentClass X" from a controller's own
// CR-reference adopt.
const clusterDepOwnerKind = "OapInstall"

// EnsureClusterDeps resolves every cluster-scoped SHARED dependency a .oap
// manifest declares (requires.clusterDeps[]) against the live cluster, in
// order, stopping at the first failure. It never CREATES a cluster dep —
// creation happens through the bundle's own CR apply, or out of band. Per dep:
//
//   - Absent AND carried as a bundled CR: neither an error nor a warning; the
//     install's own CR apply will create it.
//   - Absent AND not bundled: not a hard error (it may be provisioned out of
//     band) but never silent — returned as a warning the caller surfaces, since
//     the installed agent will not function until the dep exists.
//   - Present, pin-compatible: adopted via a metadata-only SSA patch
//     (adoptkit.Adopt), never touching the resource's spec or status.
//   - Present, pin-INCOMPATIBLE: a hard, named conflict error. Shared infra
//     referenced by other installs is never silently overwritten to make a
//     mismatched pin "fit".
//
// bundled is the set of "Kind/Name" the bundle itself carries, so an absent dep
// the install is about to create is not mistaken for a missing one. It returns
// the collected warnings and the first hard error.
//
// Pin compatibility is decided against the resource's recorded
// status.pin.digest (the common PinRecord shape v1alpha1 CRDs use). A nil
// dep.Pin, or an empty dep.Pin.Digest, asserts no pin: always compatible. A set
// dep.Pin.Digest against a live resource recording none is treated as
// compatible-but-unpinned and adopted — the manifest's pin is a drift ceiling
// the install accepts going forward, not a precondition the cluster must
// already satisfy. Only a live digest that actively DISAGREES is a conflict.
//
// owner identifies the resource on whose behalf the deps are adopted; it feeds
// the ownership annotation adoptkit.Adopt writes. c is both the existence-check
// reader and the SSA writer: oap install runs client-side against a single live
// client, so there is no label-filtered cache to route around.
func EnsureClusterDeps(ctx context.Context, c client.Client, deps []oap.RequiredClusterDep, owner types.NamespacedName, bundled map[string]bool) ([]string, error) {
	var warnings []string
	for _, dep := range deps {
		w, err := ensureClusterDep(ctx, c, dep, owner, bundled)
		if err != nil {
			return warnings, err
		}
		if w != "" {
			warnings = append(warnings, w)
		}
	}
	return warnings, nil
}

func ensureClusterDep(ctx context.Context, c client.Client, dep oap.RequiredClusterDep, owner types.NamespacedName, bundled map[string]bool) (string, error) {
	warning, present, err := inspectClusterDep(ctx, c, dep, bundled)
	if err != nil || !present {
		return warning, err
	}
	// A fresh minimal object keeps the adoption patch metadata-only.
	adoptObj := &unstructured.Unstructured{}
	adoptObj.SetGroupVersionKind(v1alpha1.SchemeGroupVersion.WithKind(dep.Kind))
	adoptObj.SetName(dep.Name)
	if err := adoptkit.Adopt(ctx, c, c, adoptObj, owner, clusterDepOwnerKind); err != nil {
		return "", fmt.Errorf("cluster dep %s/%s: adopt: %w", dep.Kind, dep.Name, err)
	}
	return "", nil
}

func inspectClusterDeps(ctx context.Context, c client.Client, deps []oap.RequiredClusterDep, bundled map[string]bool) ([]string, error) {
	var warnings []string
	for _, dep := range deps {
		warning, _, err := inspectClusterDep(ctx, c, dep, bundled)
		if err != nil {
			return nil, err
		}
		if warning != "" {
			warnings = append(warnings, warning)
		}
	}
	return warnings, nil
}

func inspectClusterDep(ctx context.Context, c client.Client, dep oap.RequiredClusterDep, bundled map[string]bool) (string, bool, error) {
	gvk := v1alpha1.SchemeGroupVersion.WithKind(dep.Kind)
	key := client.ObjectKey{Name: dep.Name} // cluster deps are cluster-scoped: no namespace

	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(gvk)
	if err := c.Get(ctx, key, live); err != nil {
		if apierrors.IsNotFound(err) {
			// Absent AND carried by the bundle: the install's own CR apply will
			// create it — nothing to adopt, nothing to warn about.
			if bundled[dep.Kind+"/"+dep.Name] {
				return "", false, nil
			}
			// Absent AND not bundled: a required shared dependency the install
			// cannot itself create. Not a hard failure (it may be provisioned out
			// of band), but never silent — surface it so the operator knows the
			// installed agent is incomplete until the dep exists.
			return fmt.Sprintf("required cluster dependency %s/%s is absent and not carried by this bundle; the installed agent will not function until it is provisioned separately", dep.Kind, dep.Name), false, nil
		}
		return "", false, fmt.Errorf("cluster dep %s/%s: get: %w", dep.Kind, dep.Name, err)
	}

	if dep.Pin != nil && dep.Pin.Digest != "" {
		recorded, _, err := unstructured.NestedString(live.Object, "status", "pin", "digest")
		if err != nil {
			return "", false, fmt.Errorf("cluster dep %s/%s: read status.pin.digest: %w", dep.Kind, dep.Name, err)
		}
		if recorded != "" && recorded != dep.Pin.Digest {
			return "", false, fmt.Errorf("cluster dep %s/%s pin mismatch: manifest wants %s, cluster has %s",
				dep.Kind, dep.Name, dep.Pin.Digest, recorded)
		}
	}

	return "", true, nil
}
