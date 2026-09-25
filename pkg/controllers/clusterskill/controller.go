// pkg/controllers/clusterskill/controller.go
//
// Package clusterskill reconciles ClusterSkill objects to surface validity and
// pin-strength as status conditions. It is the cluster-scoped mirror of the
// namespaced Skill validity controller (pkg/controllers/skill): a hand-authored
// or materialized ClusterSkill is fully described by its spec, so this
// reconciler does NOT pull or stage content — it only keeps every ClusterSkill's
// Valid/Pinned conditions honest. The ClusterSkillSource controller owns
// discovery + materialization.
package clusterskill

import (
	"context"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/internal/skillspec"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=clusterskills,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=clusterskills/status,verbs=get;update;patch

// Reconciler sets Valid + Pinned conditions on ClusterSkill objects.
type Reconciler struct {
	Client client.Client
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Skill and ClusterSkill embed the same SkillSpec/SkillStatus; the shared
	// validity + pin-strength body lives in internal/skillspec.
	return skillspec.Reconcile(ctx, r.Client, req.NamespacedName, &v1.ClusterSkill{})
}

// setPinned delegates to the shared pin-strength logic; retained as a method so
// the package's white-box unit tests address it directly.
func (r *Reconciler) setPinned(skill *v1.ClusterSkill) { skillspec.SetPinned(skill) }

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.ClusterSkill{}).
		Complete(r)
}
