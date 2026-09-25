package gke

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
)

// svcnegGVR is the GKE NEG controller's ServiceNetworkEndpointGroup resource.
var svcnegGVR = schema.GroupVersionResource{Group: "networking.gke.io", Version: "v1beta1", Resource: "servicenetworkendpointgroups"}

const negFinalizer = "networking.gke.io/neg-finalizer"

// deleteGCPNEG deletes one zonal GCP NEG via gcloud. Overridable in tests.
var deleteGCPNEG = func(ctx context.Context, rep cloud.Reporter, project, zone, name string) error {
	_, err := cloud.Gcloud(ctx, rep, "compute", "network-endpoint-groups", "delete", name,
		"--zone", zone, "--project", project, "--quiet")
	return err
}

// gcloudOnPath reports whether the gcloud CLI is available. Overridable in tests.
var gcloudOnPath = func() bool { _, err := exec.LookPath("gcloud"); return err == nil }

// UnwedgeTerminatingNamespace clears the GKE neg-finalizer that can pin a
// namespace in Terminating after its NEG-backed Service/HTTPRoute were deleted.
// See cloud.Strategy for the remediate/no-op contract.
func (Strategy) UnwedgeTerminatingNamespace(ctx context.Context, cl cloud.Clients, rep cloud.Reporter, namespace string, remediate bool) (cloud.UnwedgeReport, error) {
	var out cloud.UnwedgeReport

	list, err := cl.Dynamic.Resource(svcnegGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) || strings.Contains(strings.ToLower(err.Error()), "could not find") {
			// CRD absent (not a GKE-NEG cluster) — nothing to unwedge.
			return out, nil
		}
		return out, fmt.Errorf("list ServiceNetworkEndpointGroups in %s: %w", namespace, err)
	}

	gcloud := gcloudOnPath()

	for i := range list.Items {
		item := &list.Items[i]
		if !hasFinalizer(item.GetFinalizers(), negFinalizer) {
			continue
		}
		out.StuckFinalizers = appendUnique(out.StuckFinalizers, negFinalizer)

		negs, nerr := negSelfLinks(item)
		if nerr != nil {
			rep.Warn("cannot read backing NEGs on %s: %v", item.GetName(), nerr)
			out.Blocked = append(out.Blocked, fmt.Sprintf("%s: cannot read backing NEGs (%v) — not force-clearing", item.GetName(), nerr))
			// Still surface the manual finalizer-clear fallback for this object.
			out.ManualCommands = append(out.ManualCommands,
				fmt.Sprintf("kubectl patch svcneg %s -n %s --type merge -p '{\"metadata\":{\"finalizers\":[]}}'", item.GetName(), namespace))
			continue // never clear a finalizer whose NEGs we could not even read
		}

		// Always surface the manual commands for this object.
		for _, n := range negs {
			if p, z, name, ok := parseNEGSelfLink(n); ok {
				out.ManualCommands = append(out.ManualCommands,
					fmt.Sprintf("gcloud compute network-endpoint-groups delete %s --zone %s --project %s --quiet", name, z, p))
			}
		}
		out.ManualCommands = append(out.ManualCommands,
			fmt.Sprintf("kubectl patch svcneg %s -n %s --type merge -p '{\"metadata\":{\"finalizers\":[]}}'", item.GetName(), namespace))

		if !remediate {
			continue
		}
		if !gcloud {
			rep.Warn("gcloud not found on PATH — can't delete the orphaned GCP NEGs for %s; run the printed commands manually.", item.GetName())
			continue // degrade: leave the finalizer; manual commands already recorded
		}

		// Delete every backing NEG; only clear the finalizer if ALL are gone.
		allGone := true
		var deletedForItem int
		for _, n := range negs {
			p, z, name, ok := parseNEGSelfLink(n)
			if !ok {
				rep.Warn("unparseable NEG self-link on %s: %s", item.GetName(), n)
				out.Blocked = append(out.Blocked, fmt.Sprintf("%s: unparseable NEG self-link %q — not force-clearing", item.GetName(), n))
				allGone = false
				continue
			}
			derr := deleteGCPNEG(ctx, rep, p, z, name)
			switch {
			case derr == nil || cloud.IsGcloudNotFound(derr):
				out.CloudResourcesDeleted = appendUnique(out.CloudResourcesDeleted, n)
				deletedForItem++
			case isNEGInUse(derr):
				allGone = false
				out.Blocked = append(out.Blocked,
					fmt.Sprintf("%s: NEG %s still referenced by a live backend — not force-clearing (%v)", item.GetName(), name, derr))
				rep.Warn("NEG %s is still referenced by a live load balancer — refusing to force-clear %s's finalizer.", name, item.GetName())
			default:
				allGone = false
				out.Blocked = append(out.Blocked, fmt.Sprintf("%s: delete NEG %s failed: %v", item.GetName(), name, derr))
				rep.Warn("delete NEG %s failed: %v", name, derr)
			}
		}

		if !allGone {
			continue // safety: never clear the finalizer while a NEG remains
		}
		if err := clearFinalizer(ctx, cl, namespace, item.GetName()); err != nil {
			out.Blocked = append(out.Blocked, fmt.Sprintf("%s: clear finalizer failed: %v", item.GetName(), err))
			rep.Warn("clear finalizer on %s failed: %v", item.GetName(), err)
			continue
		}
		out.FinalizersCleared = appendUnique(out.FinalizersCleared, item.GetName())
		if deletedForItem > 0 {
			rep.OK("  cleared neg-finalizer on %s (%d backing NEG(s) deleted)", item.GetName(), deletedForItem)
		} else {
			rep.Info("  cleared neg-finalizer on %s — its status listed no backing NEGs; if a GCP NEG was orphaned, verify with: gcloud compute network-endpoint-groups list", item.GetName())
		}
	}
	return out, nil
}

// negSelfLinks returns the GCP NEG self-links from a SvcNEG's status.
// An error is returned when the status field exists but has the wrong type —
// a structural problem that must be surfaced rather than collapsed into "no NEGs".
func negSelfLinks(item *unstructured.Unstructured) ([]string, error) {
	raw, found, err := unstructured.NestedSlice(item.Object, "status", "networkEndpointGroups")
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	var out []string
	for _, e := range raw {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if s, ok := m["selfLink"].(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// clearFinalizer drops the neg-finalizer from a SvcNEG, preserving any others.
func clearFinalizer(ctx context.Context, cl cloud.Clients, namespace, name string) error {
	cur, err := cl.Dynamic.Resource(svcnegGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil // already gone
		}
		return err
	}
	remaining := make([]string, 0)
	for _, f := range cur.GetFinalizers() {
		if f != negFinalizer {
			remaining = append(remaining, f)
		}
	}
	patch := mergeFinalizersPatch(remaining)
	_, err = cl.Dynamic.Resource(svcnegGVR).Namespace(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func mergeFinalizersPatch(finalizers []string) []byte {
	if len(finalizers) == 0 {
		return []byte(`{"metadata":{"finalizers":[]}}`)
	}
	// Finalizer names are DNS-style labels (RFC 1123) and contain no characters
	// requiring JSON escaping, so manual string assembly here is safe.
	quoted := make([]string, len(finalizers))
	for i, f := range finalizers {
		quoted[i] = `"` + f + `"`
	}
	return []byte(`{"metadata":{"finalizers":[` + strings.Join(quoted, ",") + `]}}`)
}

func hasFinalizer(fs []string, want string) bool {
	for _, f := range fs {
		if f == want {
			return true
		}
	}
	return false
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

// parseNEGSelfLink extracts {project, zone, name} from a GCE network endpoint
// group self-link of the form
//
//	https://www.googleapis.com/compute/{v1,beta}/projects/PROJECT/zones/ZONE/networkEndpointGroups/NAME
//
// Returns ok=false for any link missing the projects/zones/networkEndpointGroups
// segments or a trailing name. Robust to the compute API version segment.
func parseNEGSelfLink(selfLink string) (project, zone, name string, ok bool) {
	parts := strings.Split(selfLink, "/")
	get := func(key string) (string, bool) {
		for i := 0; i < len(parts)-1; i++ {
			if parts[i] == key && parts[i+1] != "" {
				return parts[i+1], true
			}
		}
		return "", false
	}
	var okP, okZ, okN bool
	project, okP = get("projects")
	zone, okZ = get("zones")
	name, okN = get("networkEndpointGroups")
	ok = okP && okZ && okN
	if !ok {
		return "", "", "", false
	}
	return project, zone, name, true
}

// isNEGInUse reports whether a gcloud NEG-delete error means the NEG is still
// referenced by another GCP resource (a live backend service). gcloud folds the
// API reason onto the error string (see cloud.Gcloud), so a substring match is
// the reliable signal.
func isNEGInUse(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "resourceinusebyanotherresource") ||
		strings.Contains(s, "in use by another resource") ||
		strings.Contains(s, "is already being used")
}
