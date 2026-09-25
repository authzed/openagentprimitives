package wizardrun

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// refuseForeignSecret refuses to overwrite a credentials Secret this project
// did not create.
//
// Apply's writes are server-side applies with ForceOwnership, and the refusal
// in front of them covered the CHANNEL only — so a caller naming an existing
// Secret had its DATA replaced with the wizard's answers. That matters most on
// the surface with no local operator watching: admind takes the channel name
// over HTTP, and the install path's whole conflict machinery does not run here
// at all.
//
// The test is the InstalledByAnnotation, NOT mere existence. Our own Secret is
// ours to rewrite, which is exactly what a re-setup with a fresh token does, and
// refusing that would break the flow this route exists for. A Secret carrying no
// such annotation was created by something else, and no channel setup should be
// able to seize it.
//
// A nil client (an offline run) refuses nothing, matching RefuseExisting: there
// is nothing to read, and a run with no cluster cannot clobber one.
func refuseForeignSecret(ctx context.Context, c client.Client, namespace, name string) error {
	name = strings.TrimSpace(name)
	if c == nil || name == "" {
		return nil
	}
	var existing corev1.Secret
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &existing)
	switch {
	case apierrors.IsNotFound(err):
		return nil // absent → create
	case err != nil:
		return fmt.Errorf("check for existing Secret %q in namespace %q: %w", name, namespace, err)
	}
	if existing.Annotations[InstalledByAnnotation] != "" {
		return nil // ours from a prior setup → a re-setup rewrites it
	}
	return fmt.Errorf("secret %q already exists in namespace %q and was not created by this project; channel setup will not overwrite its data — choose a different channel name, or delete the Secret first",
		name, namespace)
}
