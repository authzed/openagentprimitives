package installcmd

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// workloadGVRs are the kinds whose container memory limits preserveLiveMemoryLimits
// carries forward. Both are pod templates oap ships with a bundled limit that an
// operator may have had to raise on a real cluster.
var workloadGVRs = map[string]schema.GroupVersionResource{
	"Deployment":  {Group: "apps", Version: "v1", Resource: "deployments"},
	"StatefulSet": {Group: "apps", Version: "v1", Resource: "statefulsets"},
}

// preserveLiveMemoryLimits copies each container's live
// resources.limits.memory onto d before d is applied, so a limit an operator
// raised by hand survives `oap install`. Without it the bundled limit wins
// every re-install: the operator ships at 512Mi, an install with enough
// sessions OOMKills it on startup, the operator raises the limit, and the next
// upgrade silently puts it back and the crashloop returns.
//
// Only the memory LIMIT is carried. When the bundled memory request exceeds the
// preserved limit, the request is lowered to it, since the apiserver rejects a
// request above its limit. A container absent from the live object, or one with
// no live memory limit, keeps the bundled value. A missing live object (a fresh
// install) is a no-op.
//
// The applied payload now depends on the live object, not only the bundle, but
// re-applying stays an SSA no-op: the second install reads back the value the
// first one wrote and applies it unchanged.
//
// info, when non-nil, is told about each limit that differs from the bundle.
func preserveLiveMemoryLimits(ctx context.Context, dyn dynamic.Interface, d *unstructured.Unstructured, info func(format string, args ...any)) error {
	gvr, ok := workloadGVRs[d.GetKind()]
	if !ok {
		return nil
	}
	live, err := dyn.Resource(gvr).Namespace(d.GetNamespace()).Get(ctx, d.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read live %s %s/%s to preserve its memory limits: %w", d.GetKind(), d.GetNamespace(), d.GetName(), err)
	}

	liveLimits := map[string]string{}
	liveContainers, _, _ := unstructured.NestedSlice(live.Object, "spec", "template", "spec", "containers")
	for _, raw := range liveContainers {
		c, _ := raw.(map[string]any)
		name, _, _ := unstructured.NestedString(c, "name")
		if limit, found, _ := unstructured.NestedString(c, "resources", "limits", "memory"); found && limit != "" {
			liveLimits[name] = limit
		}
	}
	if len(liveLimits) == 0 {
		return nil
	}

	containers, found, err := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
	if err != nil || !found {
		return err
	}
	for i, raw := range containers {
		c, _ := raw.(map[string]any)
		name, _, _ := unstructured.NestedString(c, "name")
		limit, ok := liveLimits[name]
		if !ok {
			continue
		}
		limitQty, err := resource.ParseQuantity(limit)
		if err != nil {
			return fmt.Errorf("%s %s/%s container %q: live memory limit %q: %w", d.GetKind(), d.GetNamespace(), d.GetName(), name, limit, err)
		}
		bundled, _, _ := unstructured.NestedString(c, "resources", "limits", "memory")
		if bundled == limit {
			continue
		}
		if err := unstructured.SetNestedField(c, limit, "resources", "limits", "memory"); err != nil {
			return err
		}
		if req, found, _ := unstructured.NestedString(c, "resources", "requests", "memory"); found {
			if reqQty, err := resource.ParseQuantity(req); err == nil && reqQty.Cmp(limitQty) > 0 {
				if err := unstructured.SetNestedField(c, limit, "resources", "requests", "memory"); err != nil {
					return err
				}
			}
		}
		containers[i] = c
		if info != nil {
			info("kept live memory limit %s on %s %s/%s container %s (bundle ships %s)", limit, d.GetKind(), d.GetNamespace(), d.GetName(), name, bundledOrNone(bundled))
		}
	}
	return unstructured.SetNestedSlice(d.Object, containers, "spec", "template", "spec", "containers")
}

func bundledOrNone(v string) string {
	if v == "" {
		return "no limit"
	}
	return v
}
