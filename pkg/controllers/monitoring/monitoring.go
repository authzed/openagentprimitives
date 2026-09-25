// pkg/controllers/monitoring/monitoring.go
//
// Reconciler watches one framework CR type for status-condition
// transitions and publishes a channelevents.MonitoringEvent on each.
// One Reconciler is registered per Target (see Register). The watchers
// reuse the operator's existing informers — they only Get, never write —
// so they need no additional RBAC.
package monitoring

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// Reconciler watches one CR type (its Target) for condition transitions.
type Reconciler struct {
	Client  client.Client
	Publish channelevents.PublishFunc
	Target  Target
	tracker *tracker
}

// Register builds and registers one watcher Reconciler per Target with
// the manager. publish is the NATS publish callback the watchers use to
// emit MonitoringEvents.
func Register(mgr ctrl.Manager, publish channelevents.PublishFunc) error {
	for _, tgt := range Targets() {
		r := &Reconciler{
			Client:  mgr.GetClient(),
			Publish: publish,
			Target:  tgt,
			tracker: newTracker(),
		}
		if err := r.SetupWithManager(mgr); err != nil {
			return fmt.Errorf("monitoring watcher %s: %w", tgt.GVKName, err)
		}
	}
	return nil
}

// SetupWithManager registers this watcher. The controller name is
// namespaced under "monitoring-" so it never collides with the primary
// controller that reconciles the same CR type.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.tracker == nil {
		r.tracker = newTracker()
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("monitoring-" + strings.ToLower(r.Target.GVKName)).
		For(r.Target.New()).
		Complete(r)
}

// Reconcile reads the target object's conditions and publishes a
// MonitoringEvent on each failure or recovery transition.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	obj := r.Target.New()
	if err := r.Client.Get(ctx, req.NamespacedName, obj); err != nil {
		if apierrors.IsNotFound(err) {
			// Object gone — drop its tracker entries so a recreate is
			// evaluated fresh and we don't emit a spurious recovery.
			r.tracker.forget(req.Namespace + "/" + req.Name + "/")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	conds := r.Target.GetConditions(obj)
	for _, rule := range r.Target.Rules {
		c := meta.FindStatusCondition(conds, rule.ConditionType)
		isFailing := c != nil && c.Status == rule.BadStatus
		key := req.Namespace + "/" + req.Name + "/" + rule.ConditionType
		emit, transition := r.tracker.observe(key, isFailing, rule.Terminal)
		if !emit {
			continue
		}
		ev := buildEvent(r.Target, rule, req, c, transition)
		if err := channelevents.PublishMonitoring(r.Publish, ev); err != nil {
			// Monitoring is best-effort — log, never fail the reconcile.
			logger.Info("publish MonitoringEvent failed",
				"condition", rule.ConditionType, "transition", transition, "err", err.Error())
		}
	}
	return ctrl.Result{}, nil
}

// buildEvent assembles a MonitoringEvent from the matched rule and the
// current condition. c may be nil on a "recovered" transition where the
// condition was removed entirely rather than flipped to a good status.
func buildEvent(tgt Target, rule Rule, req ctrl.Request, c *metav1.Condition, transition string) channelevents.MonitoringEvent {
	ev := channelevents.MonitoringEvent{
		Level:      rule.Level,
		Category:   rule.Category,
		Transition: transition,
		Source: channelevents.MonitoringSourceRef{
			Kind:      tgt.GVKName,
			Namespace: req.Namespace,
			Name:      req.Name,
		},
		Condition: rule.ConditionType,
		Timestamp: time.Now().UTC(),
	}
	if c != nil {
		ev.Reason = c.Reason
		ev.Summary = c.Message
		if !c.LastTransitionTime.IsZero() {
			ev.Timestamp = c.LastTransitionTime.Time.UTC()
		}
	}
	if transition == channelevents.MonitoringTransitionFailed && rule.Hint != "" {
		ev.Hint = strings.ReplaceAll(rule.Hint, "{name}", req.Name)
	}
	return ev
}
