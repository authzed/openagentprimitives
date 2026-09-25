package sandboxkinds

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// Shared shape for backends whose sandbox is a namespaced Kubernetes object.
// A backend outside the cluster uses none of this — Handle.Ref stays opaque,
// and this file is a convenience for the kinds that happen to agree, not a
// widening of the seam.

// NamespacedRef renders a namespaced object's identity as a Handle.Ref.
func NamespacedRef(namespace, name string) string { return namespace + "/" + name }

// ParseNamespacedRef splits a "<namespace>/<name>" Handle.Ref.
func ParseNamespacedRef(ref string) (namespace, name string, err error) {
	ns, n, ok := strings.Cut(ref, "/")
	if !ok || ns == "" || n == "" {
		return "", "", fmt.Errorf("malformed sandbox handle ref %q: want \"<namespace>/<name>\"", ref)
	}
	return ns, n, nil
}

// ResolveHandle validates that h belongs to kind and splits its ref. A handle
// owned by another kind is an error rather than a best-effort read: a class
// that flips kinds mid-life must not have its old handle interpreted by the
// new backend, whose ref format may coincide while meaning something else.
func ResolveHandle(h Handle, kind string) (namespace, name string, err error) {
	if h.Kind != kind {
		return "", "", fmt.Errorf("handle belongs to sandbox kind %q, not %q", h.Kind, kind)
	}
	return ParseNamespacedRef(h.Ref)
}

// RequireWorkspaceClaim reports whether s's shared workspace claim exists yet,
// returning an error wrapping ErrPreconditionPending when it does not.
//
// Creating a sandbox that mounts a missing PVC yields "FailedScheduling: PVC
// not found", which under restartPolicy:Never becomes a terminal pod failure
// and surfaces to the user as a spurious BundleFailed.
//
// It waits for the claim to EXIST and deliberately NOT for it to be Bound: the
// claim's StorageClass is WaitForFirstConsumer, so the sandbox pod is the
// consumer that triggers binding. Gating on Bound would wait for something
// that can only happen after this returns — a deadlock, not a slow path.
func RequireWorkspaceClaim(ctx context.Context, c client.Client, s *v1alpha1.SpiceboxSession) error {
	w := s.Spec.Workspace
	if w.Mode != v1alpha1.WorkspaceShared || w.SharedClaimName == "" {
		return nil
	}
	var claim corev1.PersistentVolumeClaim
	err := c.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: w.SharedClaimName}, &claim)
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("workspace claim %q does not exist yet: %w", w.SharedClaimName, ErrPreconditionPending)
	case err != nil:
		return fmt.Errorf("get workspace claim %q: %w", w.SharedClaimName, err)
	}
	return nil
}
