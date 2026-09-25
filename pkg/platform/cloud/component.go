package cloud

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/utils/ptr"

	"github.com/authzed/openagentprimitives/pkg/platform/kubeyaml"
)

const (
	// InstalledByAnnotation marks every resource applyDoc applies, so 'oap clean'
	// can distinguish what oap installed (e.g. cert-manager, Envoy Gateway) from
	// pre-existing cluster infrastructure it must NOT remove. The value is
	// identical to cmd/oap/internal/kube.InstalledByAnnotation so 'oap clean'
	// matches both paths.
	InstalledByAnnotation = "agentprimitives.authzed.com/installed-by"
	// InstalledByValue is the value stamped by applyDoc.
	InstalledByValue = "ap"
)

// IsNoMatchError reports a "the server doesn't have a resource type" error,
// which is how a missing CRD surfaces through the dynamic client. Exported so
// both certmanager sub-package and cmd/oap can share the same predicate.
func IsNoMatchError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "could not find the requested resource")
}

// Component describes a cluster-wide dependency that EnsureComponent can install
// on the operator's behalf (e.g. cert-manager, Envoy Gateway). The cmd/oap layer
// builds per-cloud Component values using its embedded manifests and hands them to
// EnsureComponent.
type Component struct {
	Name      string                              // human name for prompts/logs
	Why       string                              // one-line reason it's needed
	Creates   string                              // what installing it adds (namespaces/CRDs/etc.)
	ManualCmd string                              // exact command to run instead
	Present   func(context.Context) (bool, error) // already installed?
	Manifests func() ([][]byte, error)            // embedded bundle (one []byte per doc group)
	Ready     func(context.Context) error         // post-apply readiness wait; nil means skip
}

// EnsureComponent offers to install comp if not already present, after explaining
// what it will do and (unless assumeYes) getting consent. Returns present=true
// once the component is usable. On decline it prints the manual command and
// returns (false, nil) so the caller can skip dependent steps.
//
// rep.Interactive() replaces the old isTTY bool: when false, the prompt is never
// shown and the operation is declined (suitable for CI / non-TTY contexts unless
// assumeYes is true).
func EnsureComponent(ctx context.Context, rep Reporter, in io.Reader, cl Clients, comp Component, assumeYes bool) (bool, error) {
	ok, err := comp.Present(ctx)
	if err != nil {
		return false, fmt.Errorf("check %s: %w", comp.Name, err)
	}
	if ok {
		rep.Info("%s already present", comp.Name)
		return true, nil
	}
	rep.Info("\n%s is not installed.\n  why: %s\n  installing it creates: %s\n  manual alternative: %s",
		comp.Name, comp.Why, comp.Creates, comp.ManualCmd)
	if !Confirm(in, rep, fmt.Sprintf("Install %s now?", comp.Name), assumeYes) {
		rep.Info("  skipped %s; run the command above, then re-run oap install.", comp.Name)
		return false, nil
	}
	groups, err := comp.Manifests()
	if err != nil {
		return false, fmt.Errorf("load %s manifests: %w", comp.Name, err)
	}
	rep.Step("apply %s", comp.Name)
	for _, g := range groups {
		docs, derr := kubeyaml.Split(g)
		if derr != nil {
			return false, fmt.Errorf("split %s manifest: %w", comp.Name, derr)
		}
		for _, d := range docs {
			// Make the component leader-election-free so it works on GKE
			// Autopilot, where the bundled cert-manager/Envoy default of a
			// kube-system leader-election lease is forbidden (cainjector then
			// never leads → the webhook CA is never injected).
			if err := disableLeaderElectionDoc(d); err != nil {
				return false, fmt.Errorf("%s: %w", comp.Name, err)
			}
			if err := applyDoc(ctx, cl.Dynamic, d, "ap-install"); err != nil {
				return false, fmt.Errorf("apply %s %s/%s: %w", comp.Name, d.GetKind(), d.GetName(), err)
			}
		}
	}
	if comp.Ready != nil {
		if err := comp.Ready(ctx); err != nil {
			return false, fmt.Errorf("%s did not become ready: %w", comp.Name, err)
		}
	}
	rep.Info("  installed %s", comp.Name)
	return true, nil
}

// Confirm returns the operator's y/N answer. assumeYes short-circuits to
// true; a non-interactive reporter without assumeYes is always false (never block
// CI on a prompt); otherwise it reads one line from in and accepts only y/yes.
//
// The prompt is emitted through rep.Suspend so the install checklist quiesces and
// clears its live region for the duration: the "[y/N]:" and the user's typed
// answer stay together on one line instead of being separated by redrawn
// checklist rows (the garbled-prompt bug). The prompt carries no trailing newline
// so the cursor rests right after "[y/N]:".
func Confirm(in io.Reader, rep Reporter, prompt string, assumeYes bool) bool {
	if assumeYes {
		return true
	}
	if !rep.Interactive() {
		return false
	}
	var yes bool
	rep.Suspend(func(out io.Writer, _ io.Reader) {
		fmt.Fprintf(out, "%s [y/N]: ", prompt)
		line, _ := bufio.NewReader(in).ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			yes = true
		}
	})
	return yes
}

// applyDoc does a server-side apply of obj via the dynamic client. Falls back to
// Create on NotFound (the fake dynamic client returns NotFound for ApplyPatch on
// missing objects in tests). Any OTHER patch rejection is returned as an error:
// the Get+Update fallback that would otherwise rewrite the live object is gated
// behind the test-only applyUpdateFallback (see its doc comment).
//
// applyDoc stamps InstalledByAnnotation on every object it applies, mirroring
// cmd/oap/internal/kube.Apply so 'oap clean' can remove pkg/cloud-applied objects
// (cert-manager, Envoy Gateway, NetworkPolicies) just like ap-install manifests.
func applyDoc(ctx context.Context, dyn dynamic.Interface, obj *unstructured.Unstructured, fieldManager string) error {
	gvk := obj.GroupVersionKind()
	gvr, namespaced, err := resolveDocGVR(gvk)
	if err != nil {
		return fmt.Errorf("apply %s/%s: resolve GVR: %w", gvk.Kind, obj.GetName(), err)
	}

	// Stamp provenance so 'oap clean' can tell oap-installed objects from
	// pre-existing cluster infra.
	anns := obj.GetAnnotations()
	if anns == nil {
		anns = map[string]string{}
	}
	anns[InstalledByAnnotation] = InstalledByValue
	obj.SetAnnotations(anns)

	data, err := json.Marshal(obj.Object)
	if err != nil {
		return fmt.Errorf("apply %s/%s: marshal: %w", gvk.Kind, obj.GetName(), err)
	}

	patchOpts := metav1.PatchOptions{FieldManager: fieldManager, Force: ptr.To(true)}

	var ri dynamic.ResourceInterface
	if namespaced {
		if obj.GetNamespace() == "" {
			return fmt.Errorf("apply %s/%s: missing metadata.namespace on a namespaced resource",
				gvk.Kind, obj.GetName())
		}
		ri = dyn.Resource(gvr).Namespace(obj.GetNamespace())
	} else {
		ri = dyn.Resource(gvr)
	}

	_, patchErr := ri.Patch(ctx, obj.GetName(), types.ApplyPatchType, data, patchOpts)
	if patchErr == nil {
		return nil
	}
	if apierrors.IsNotFound(patchErr) {
		_, err = ri.Create(ctx, obj, metav1.CreateOptions{FieldManager: fieldManager})
		if err != nil {
			return fmt.Errorf("apply %s/%s: create: %w", gvk.Kind, obj.GetName(), err)
		}
		return nil
	}
	if !applyUpdateFallback {
		return fmt.Errorf("apply %s/%s: %w", gvk.Kind, obj.GetName(), patchErr)
	}

	// TEST-ONLY (see applyUpdateFallback): the dynamic fake rejects apply
	// patches on existing objects, so tests fall back to a plain Update.
	_, getErr := ri.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if getErr != nil {
		// Object doesn't exist — forward the original patch error.
		return fmt.Errorf("apply %s/%s: %w", gvk.Kind, obj.GetName(), patchErr)
	}
	_, err = ri.Update(ctx, obj, metav1.UpdateOptions{FieldManager: fieldManager})
	if err != nil {
		return fmt.Errorf("apply %s/%s: update: %w", gvk.Kind, obj.GetName(), err)
	}
	return nil
}

// applyUpdateFallback enables applyDoc's Get+Update fallback after a failed
// server-side apply. It is FALSE in production and must stay that way: that
// fallback replaces the live object with the manifest — no resourceVersion, no
// managedFields — discarding controller-added finalizers, ownerReferences,
// injected webhook caBundles, and every field another manager owns, then
// returns nil so the caller is told "applied". A re-install reaches every
// object applyDoc touches: the offered cert-manager / Envoy Gateway bundles
// and, via applyNetworkPolicy, the allow-dns-cloud and allow-webd-gateway
// NetworkPolicies. Silently rewriting a NetworkPolicy is rewriting a security
// control.
//
// It exists only because client-go's dynamic fake cannot serve an apply patch
// for *unstructured.Unstructured. The only switch that sets it is
// SetApplyUpdateFallbackForTest in export_test.go, so no production build can
// turn it on. Mirrors the identical gate on cmd/oap/internal/kube.Apply.
var applyUpdateFallback bool

// resolveDocGVR maps a GVK to its plural REST resource. Mirrors the static table
// in cmd/oap/internal/kube for the well-known core kinds expected in component
// bundles (cert-manager, Envoy Gateway). Falls back to a lower-case pluralized
// heuristic for less-common kinds.
func resolveDocGVR(gvk schema.GroupVersionKind) (schema.GroupVersionResource, bool, error) {
	switch gvk.Kind {
	case "Namespace":
		return schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}, false, nil
	case "ServiceAccount":
		return schema.GroupVersionResource{Group: "", Version: "v1", Resource: "serviceaccounts"}, true, nil
	case "Secret":
		return schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}, true, nil
	case "ConfigMap":
		return schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}, true, nil
	case "Service":
		return schema.GroupVersionResource{Group: "", Version: "v1", Resource: "services"}, true, nil
	case "Deployment":
		return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, true, nil
	case "StatefulSet":
		return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}, true, nil
	case "Role":
		return schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}, true, nil
	case "RoleBinding":
		return schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}, true, nil
	case "ClusterRole":
		return schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}, false, nil
	case "ClusterRoleBinding":
		return schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}, false, nil
	case "CustomResourceDefinition":
		return schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}, false, nil
	case "ValidatingWebhookConfiguration":
		return schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingwebhookconfigurations"}, false, nil
	case "MutatingWebhookConfiguration":
		return schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "mutatingwebhookconfigurations"}, false, nil
	case "StorageClass":
		return schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}, false, nil
	case "NetworkPolicy":
		return schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"}, true, nil
	}
	// Heuristic for custom or less-common kinds. Assumes namespaced; cluster-scoped
	// kinds not in the table above must be added explicitly.
	lower := strings.ToLower(gvk.Kind)
	var plural string
	switch {
	case len(lower) == 0:
		plural = lower
	case lower[len(lower)-1] == 's':
		plural = lower + "es"
	case lower[len(lower)-1] == 'y':
		plural = lower[:len(lower)-1] + "ies"
	default:
		plural = lower + "s"
	}
	return schema.GroupVersionResource{Group: gvk.Group, Version: gvk.Version, Resource: plural}, true, nil
}

// disableLeaderElectionDoc appends --leader-elect=false to any Deployment container
// that runs with a --leader-election-namespace flag. The bundled cert-manager and
// Envoy Gateway default leader-election to kube-system, which GKE Autopilot forbids
// writing to — so the cainjector/controller never acquire leadership and (for
// cert-manager) the validating webhook's CA is never injected. No-op for
// non-Deployment docs and containers without the flag.
func disableLeaderElectionDoc(doc *unstructured.Unstructured) error {
	if doc.GetKind() != "Deployment" {
		return nil
	}
	containers, found, err := unstructured.NestedSlice(doc.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return fmt.Errorf("disable leader election: read %s containers: %w", doc.GetName(), err)
	}
	if !found {
		return nil
	}
	changed := false
	for i, c := range containers {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		args, _ := cm["args"].([]any)
		usesLeaderElection, alreadyDisabled := false, false
		for _, a := range args {
			s, _ := a.(string)
			if strings.HasPrefix(s, "--leader-election-namespace") {
				usesLeaderElection = true
			}
			if s == "--leader-elect=false" {
				alreadyDisabled = true
			}
		}
		if usesLeaderElection && !alreadyDisabled {
			cm["args"] = append(args, "--leader-elect=false")
			containers[i] = cm
			changed = true
		}
	}
	if changed {
		if err := unstructured.SetNestedSlice(doc.Object, containers, "spec", "template", "spec", "containers"); err != nil {
			return fmt.Errorf("disable leader election: write %s args: %w", doc.GetName(), err)
		}
	}
	return nil
}
