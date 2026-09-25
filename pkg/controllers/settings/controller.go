package settings

import (
	"context"
	"fmt"
	"sort"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	webhooksettings "github.com/authzed/openagentprimitives/pkg/controllers/webhooks/settings"
	"github.com/authzed/openagentprimitives/pkg/platform/preferences"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=clusteragentsettings,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=clusteragentsettings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentsettings,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentsettings/status,verbs=get;update;patch

// ClusterReconciler reconciles ClusterAgentSettings.
type ClusterReconciler struct {
	Client          client.Client
	RevokePublisher *RevokePublisher
}

func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cas spiceboxv1alpha1.ClusterAgentSettings
	if err := r.Client.Get(ctx, req.NamespacedName, &cas); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Diff the allowlists against the durable per-CR record
	// (status.observedLimits, cached in-process), emit tool-origin revokes for
	// explicit removals, and stamp the advanced record for the status write
	// below to persist.
	//
	// The error is CARRIED, not returned here: a revoke that failed to publish
	// must not stop this CR's status from converging. The publisher has already
	// held the failed origins in both its cache and the stamped record, so
	// returning the error after the status write is what turns "held back" into
	// "retried" — the requeue is the only thing that re-runs the diff.
	var revokeErr error
	if r.RevokePublisher != nil {
		var mcpAllowed *[]spiceboxv1alpha1.AllowedMCPServer
		var toolkitsAllowed *[]string
		if cas.Spec.Limits != nil {
			mcpAllowed = cas.Spec.Limits.AllowedMCPServers
			toolkitsAllowed = cas.Spec.Limits.AllowedToolkits
		}
		revokeErr = r.RevokePublisher.Observe(ctx, mcpAllowed, toolkitsAllowed, &cas.Status, cas.Name, "")
	}
	cas.Status.ObservedGeneration = cas.Generation
	stampSelfConsistent(&cas, &cas.Status.Conditions, cas.Name, spiceboxv1alpha1.ClusterAgentSettingsName, &cas.Spec)
	if err := r.Client.Status().Update(ctx, &cas); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, revokeErr
}

func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
	return ctrl.NewControllerManagedBy(mgr).For(&spiceboxv1alpha1.ClusterAgentSettings{}).Complete(r)
}

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentclasses,verbs=get;list;watch

// NamespaceReconciler reconciles AgentSettings.
type NamespaceReconciler struct {
	Client          client.Client
	RevokePublisher *RevokePublisher
}

func (r *NamespaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var as spiceboxv1alpha1.AgentSettings
	if err := r.Client.Get(ctx, req.NamespacedName, &as); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// See ClusterReconciler.Reconcile for why the revoke error is carried past
	// the status write rather than returned in its place.
	var revokeErr error
	if r.RevokePublisher != nil {
		var mcpAllowed *[]spiceboxv1alpha1.AllowedMCPServer
		var toolkitsAllowed *[]string
		if as.Spec.Limits != nil {
			mcpAllowed = as.Spec.Limits.AllowedMCPServers
			toolkitsAllowed = as.Spec.Limits.AllowedToolkits
		}
		revokeErr = r.RevokePublisher.Observe(ctx, mcpAllowed, toolkitsAllowed, &as.Status, as.Namespace+"/"+as.Name, as.Namespace)
	}
	as.Status.ObservedGeneration = as.Generation
	stampSelfConsistent(&as, &as.Status.Conditions, as.Name, spiceboxv1alpha1.AgentSettingsName, &as.Spec)
	if msg := r.classPreferencesViolations(ctx, as.Namespace, as.Spec.ClassUserPreferences); msg == "" {
		conditions.SetTrue(&as, &as.Status.Conditions,
			spiceboxv1alpha1.SettingsConditionClassPreferencesValid,
			spiceboxv1alpha1.ReasonClassPreferencesValid)
	} else {
		conditions.SetFalse(&as, &as.Status.Conditions,
			spiceboxv1alpha1.SettingsConditionClassPreferencesValid,
			spiceboxv1alpha1.ReasonClassPreferencesInvalid, msg)
	}
	if err := r.Client.Status().Update(ctx, &as); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, revokeErr
}

// classPreferencesViolations cross-checks classUserPreferences globals against
// INSTALLED class schemas. An unknown class is tolerated (a global may precede
// its class's install, an ordinary install-order race); an unknown key or a
// type mismatch against an installed class is a violation. Returns "" when
// clean, else a sorted, joined message.
func (r *NamespaceReconciler) classPreferencesViolations(ctx context.Context,
	ns string, globals map[string]map[string]spiceboxv1alpha1.PreferenceGlobal) string {

	var msgs []string
	for className, prefs := range globals {
		var ac spiceboxv1alpha1.AgentClass
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: className}, &ac); err != nil {
			if apierrors.IsNotFound(err) {
				continue // install-order tolerance
			}
			// No silent errors: a read failure is a violation message, not a skip.
			msgs = append(msgs, fmt.Sprintf("%s: reading class: %v", className, err))
			continue
		}
		byName := map[string]spiceboxv1alpha1.UserPreferenceSchema{}
		for _, s := range ac.Spec.UserPreferences {
			byName[s.Name] = s
		}
		for key, g := range prefs {
			sch, ok := byName[key]
			if !ok {
				msgs = append(msgs, fmt.Sprintf("%s.%s: key not declared in the class's userPreferences", className, key))
				continue
			}
			if err := preferences.ValidateValue(sch, g.Value); err != nil {
				msgs = append(msgs, fmt.Sprintf("%s.%s: %v", className, key, err))
			}
		}
	}
	sort.Strings(msgs)
	return strings.Join(msgs, "; ")
}

func (r *NamespaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
	mapClassToSettings := func(ctx context.Context, o client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{
			Namespace: o.GetNamespace(), Name: spiceboxv1alpha1.AgentSettingsName}}}
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&spiceboxv1alpha1.AgentSettings{}).
		Watches(&spiceboxv1alpha1.AgentClass{}, handler.EnqueueRequestsFromMapFunc(mapClassToSettings)).
		Complete(r)
}

// stampSelfConsistent flags singleton + self-consistency on the SelfConsistent
// condition. Self-consistency: resolve the spec's own defaults against its own
// limits; any allowlist/clamp violation means a default contradicts a ceiling.
func stampSelfConsistent(obj conditions.Generationer, conds *[]metav1.Condition, name, wellKnown string, spec *spiceboxv1alpha1.SettingsSpec) {
	if name != wellKnown {
		conditions.SetFalse(obj, conds, spiceboxv1alpha1.SettingsConditionSelfConsistent,
			spiceboxv1alpha1.ReasonSettingsNotSingleton,
			fmt.Sprintf("name must be %q (singleton); %q is ignored by resolution", wellKnown, name))
		return
	}
	if msg := webhooksettings.SelfConsistencyError(spec); msg != "" {
		conditions.SetFalse(obj, conds, spiceboxv1alpha1.SettingsConditionSelfConsistent,
			spiceboxv1alpha1.ReasonSettingsSelfInconsistent, msg)
		return
	}
	conditions.SetTrue(obj, conds, spiceboxv1alpha1.SettingsConditionSelfConsistent, spiceboxv1alpha1.ReasonSettingsConsistent)
}
