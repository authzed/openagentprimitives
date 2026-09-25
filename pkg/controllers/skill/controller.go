// pkg/controllers/skill/controller.go
//
// Package skill reconciles Skill objects to surface validity and pin-strength
// as status conditions. It does NOT pull or stage content — a hand-authored
// Skill is fully described by its spec. The SkillSource controller
// owns discovery + materialization; this reconciler just keeps every Skill's
// Valid/Pinned conditions honest.
package skill

import (
	"context"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/internal/skillspec"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=skills,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=skills/status,verbs=get;update;patch

// Reconciler sets Valid + Pinned conditions on Skill objects.
type Reconciler struct {
	Client client.Client
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Skill and ClusterSkill embed the same SkillSpec/SkillStatus; the shared
	// validity + pin-strength body lives in internal/skillspec.
	return skillspec.Reconcile(ctx, r.Client, req.NamespacedName, &v1.Skill{})
}

// setPinned delegates to the shared pin-strength logic; retained as a method so
// the package's white-box unit tests address it directly.
func (r *Reconciler) setPinned(skill *v1.Skill) { skillspec.SetPinned(skill) }

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.Skill{}).
		Complete(r)
}
