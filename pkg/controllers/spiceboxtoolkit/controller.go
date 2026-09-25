// Package spiceboxtoolkit reconciles SpiceboxToolkit CRs. The reconciler
// detects collisions with built-in toolkits, stamps a cli-kind pin baseline
// from the declared ToolkitTarget fields, and surfaces all findings as
// conditions.
package spiceboxtoolkit

import (
	"context"
	"fmt"
	"net/url"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	clikind "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/cli"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
	"github.com/authzed/openagentprimitives/pkg/controllers/internal/skillpin"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/registry"
)

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=spiceboxtoolkits,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=spiceboxtoolkits/status,verbs=get;update;patch

// Reconciler reconciles SpiceboxToolkit objects.
type Reconciler struct {
	Client   client.Client
	Registry *registry.Registry
	// RevokePublisher, when non-nil, receives tool-origin revocation events
	// on CR deletion so running sessions deny that origin immediately.
	// Best-effort: a nil publisher is tolerated for local-dev / no-NATS runs.
	RevokePublisher *revocation.Publisher
}

// SetupWithManager registers the reconciler with the manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := ctrl.NewControllerManagedBy(mgr).
		For(&spiceboxv1alpha1.SpiceboxToolkit{}).
		Complete(r); err != nil {
		return err
	}
	if r.RevokePublisher != nil {
		inf, err := mgr.GetCache().GetInformer(context.Background(), &spiceboxv1alpha1.SpiceboxToolkit{})
		if err != nil {
			return err
		}
		revokeLog := ctrl.Log.WithName("spiceboxtoolkit-revoke")
		if _, err := inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
			DeleteFunc: func(obj any) {
				cr := SpiceboxToolkitFromDelete(obj)
				if cr == nil {
					return
				}
				if cr.Spec.Name == "" {
					revokeLog.Info("skipping tool-origin revoke: Spec.Name is empty", "toolkit", cr.Name)
					return
				}
				key, scope := SpiceboxToolkitRevokeKeyScope(cr)
				if emitErr := r.RevokePublisher.Emit(context.Background(), "tool-origin", key, scope); emitErr != nil {
					revokeLog.Info("emit tool-origin revoke on delete failed", "toolkit", cr.Spec.Name, "err", emitErr.Error())
				}
			},
		}); err != nil {
			return err
		}
	}
	return nil
}

// Reconcile checks whether the SpiceboxToolkit CR collides with a built-in and
// sets the Valid condition accordingly.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var tk spiceboxv1alpha1.SpiceboxToolkit
	if cont, err := apreconcile.LoadInto(ctx, r.Client, req.NamespacedName, &tk); !cont {
		return ctrl.Result{}, err
	}
	if tk.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	// Stamp pin before any early return: pin identity is spec-derived and
	// orthogonal to validity. A siteURL-invalid toolkit still has a declared
	// binary hash / version range the settings gate needs to evaluate.
	tk.Status.Pin = cliPin(tk.Spec.Target, tk.Status.Pin)

	if err := validateSiteURL(tk.Spec.SiteURL); err != nil {
		tk.Status.ObservedGeneration = tk.Generation
		conditions.Set(&tk, &tk.Status.Conditions, metav1.Condition{
			Type:    spiceboxv1alpha1.SpiceboxToolkitConditionValid,
			Status:  metav1.ConditionFalse,
			Reason:  spiceboxv1alpha1.ReasonSpecInvalid,
			Message: err.Error(),
		})
		return ctrl.Result{}, r.Client.Status().Update(ctx, &tk)
	}

	cond := metav1.Condition{
		Type:   spiceboxv1alpha1.SpiceboxToolkitConditionValid,
		Status: metav1.ConditionTrue,
		Reason: "Resolved",
	}
	if collides(&tk, r.Registry) {
		cond.Status = metav1.ConditionFalse
		cond.Reason = spiceboxv1alpha1.ReasonBuiltinCollision
		cond.Message = "a built-in toolkit with the same (name, toolkitRevision) is shipped with the operator; rename or change the revision"
	}

	tk.Status.ObservedGeneration = tk.Generation
	conditions.Set(&tk, &tk.Status.Conditions, cond)
	return ctrl.Result{}, r.Client.Status().Update(ctx, &tk)
}

// validateSiteURL ensures SiteURL, when set, is a parseable http or
// https URL. Empty string is allowed (the field is +optional).
func validateSiteURL(siteURL string) error {
	if siteURL == "" {
		return nil
	}
	u, err := url.Parse(siteURL)
	if err != nil {
		return fmt.Errorf("siteURL %q: %w", siteURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("siteURL %q: scheme must be http or https (got %q)", siteURL, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("siteURL %q: missing host", siteURL)
	}
	return nil
}

func collides(tk *spiceboxv1alpha1.SpiceboxToolkit, reg *registry.Registry) bool {
	for _, b := range reg.Builtins() {
		if b.Name == tk.Spec.Name && b.ToolkitRevision == tk.Spec.ToolkitRevision {
			return true
		}
	}
	return false
}

// cliPin builds the cli-kind PinRecord from the ToolkitTarget spec fields,
// preserving ObservedAt when the declared identity is unchanged from prev.
// Delegates classification to clikind.StrengthFor — the shared rule used
// by both this controller and the settings wiring.
func cliPin(target spiceboxv1alpha1.ToolkitTarget, prev *spiceboxv1alpha1.PinRecord) *spiceboxv1alpha1.PinRecord {
	strength := clikind.StrengthFor(target.PinnedBinaryHash, target.VersionRange)
	return skillpin.Declared(clikind.KindName, string(strength), target.PinnedBinaryHash, target.VersionRange, prev)
}
