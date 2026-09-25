package skillspec

import (
	"context"
	"fmt"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
	"github.com/authzed/openagentprimitives/pkg/controllers/internal/skillpin"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/canonical"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/materialize"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/validate"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Object is the shared contract satisfied by both *v1.Skill and *v1.ClusterSkill:
// a Kubernetes object (loadable / status-updatable) that also exposes the
// validity-condition Generation and pointers to its embedded SkillSpec / SkillStatus.
// The Skill and ClusterSkill CRDs carry the SAME SkillSpec / SkillStatus, so a
// single reconcile body drives both mirror controllers.
type Object interface {
	client.Object
	conditions.Generationer
	SkillSpec() *v1.SkillSpec
	SkillStatus() *v1.SkillStatus
}

// Reconcile is the shared validity + pin-strength reconcile body for the Skill
// and ClusterSkill controllers. Neither pulls or stages content — the CR is
// fully described by its spec — so this only keeps the Valid / Pinned conditions
// and pin status honest. It loads obj by key, skips deleting objects, re-runs the
// admission rule set (incl. the provenance check), records pin strength, and
// persists status. Behavior is identical to the per-controller bodies it replaces.
func Reconcile(ctx context.Context, c client.Client, key client.ObjectKey, obj Object) (ctrl.Result, error) {
	if cont, err := apreconcile.LoadInto(ctx, c, key, obj); !cont {
		return ctrl.Result{}, err
	}
	if obj.GetDeletionTimestamp() != nil {
		return ctrl.Result{}, nil
	}

	spec := obj.SkillSpec()
	status := obj.SkillStatus()

	// Validity: re-run the same rule set the admission webhook enforces,
	// including the provenance check (a hand-authored skill must not claim a
	// git-authority canonical name). This reconcile is the DURABLE backstop —
	// the webhook is failurePolicy=Ignore (best-effort) — so the "materialized?"
	// verdict MUST come from the same unforgeable owner-ref signal the webhook
	// uses (pkg/tools/skills/materialize), never from the user-writable spec.Source
	// field: a tenant with create access could set spec.Source to anything.
	errs := validate.Skill(spec.CanonicalName, spec.Frontmatter.Name,
		spec.Description, spec.Body)

	materialized, matErr := materializedGitAuthority(ctx, c, obj)
	if matErr != nil {
		// An inconclusive owner lookup (real API error, not NotFound) must not be
		// silently treated as either verdict. Log it, fail closed for this pass
		// (materialized stays false, so a non-local name is denied), persist that
		// verdict, and propagate the error too so controller-runtime retries with
		// backoff instead of waiting for an unrelated spec change to re-trigger.
		log.FromContext(ctx).Info("skill materialization check failed; treating as unmaterialized this pass",
			"skill", obj.GetName(), "namespace", obj.GetNamespace(),
			"canonicalName", spec.CanonicalName, "err", matErr.Error())
	}
	if err := validate.CheckProvenance(spec.CanonicalName, materialized); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		conditions.SetFalse(obj, &status.Conditions,
			v1.SkillConditionValid, v1.ReasonSkillInvalidSpec, errs[0].Error())
	} else {
		conditions.SetTrue(obj, &status.Conditions,
			v1.SkillConditionValid, v1.ReasonSkillValid)
	}

	// Pin strength: advisory. Unpinned → warn (rolling).
	SetPinned(obj)

	status.ObservedGeneration = obj.GetGeneration()
	if err := c.Status().Update(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, matErr
}

// materializedGitAuthority dispatches to the shared materialize.Skill /
// materialize.ClusterSkill check by the concrete type behind Object: a Skill
// is namespaced and owned by a same-namespace SkillSource, a ClusterSkill is
// cluster-scoped and owned by a ClusterSkillSource. The two differ in scope,
// so this can't be expressed through the Object interface alone (and the
// leaf helpers in skillspec.go deliberately never touch the parent CR type).
func materializedGitAuthority(ctx context.Context, r client.Reader, obj Object) (bool, error) {
	canonicalName := obj.SkillSpec().CanonicalName
	switch obj.(type) {
	case *v1.Skill:
		return materialize.Skill(ctx, r, obj.GetNamespace(), obj.GetOwnerReferences(), canonicalName)
	case *v1.ClusterSkill:
		return materialize.ClusterSkill(ctx, r, obj.GetOwnerReferences(), canonicalName)
	default:
		return false, fmt.Errorf("skillspec: materialization check unsupported for %T", obj)
	}
}

// SetPinned records the PinRecord on obj's status from its canonical name, and
// sets the advisory Pinned condition. Shared by the Skill and ClusterSkill
// controllers (and their unit tests).
func SetPinned(obj Object) {
	spec := obj.SkillSpec()
	status := obj.SkillStatus()
	n, err := canonical.Parse(spec.CanonicalName)
	if err != nil {
		// Unparseable name → record nothing useful; the Valid=False condition
		// already carries the parse error.
		status.Pin = nil
		conditions.SetFalse(obj, &status.Conditions,
			v1.SkillConditionPinned, v1.ReasonSkillInvalidSpec, err.Error())
		return
	}
	strength := n.PinStrength()
	status.Pin = skillpin.Baseline(n, status.Pin)

	switch strength {
	case canonical.PinFrozen:
		conditions.SetTrue(obj, &status.Conditions,
			v1.SkillConditionPinned, v1.ReasonSkillPinFrozen)
	case canonical.PinNamed:
		conditions.SetTrue(obj, &status.Conditions,
			v1.SkillConditionPinned, v1.ReasonSkillPinNamed)
	default: // PinUnpinned
		conditions.SetFalse(obj, &status.Conditions,
			v1.SkillConditionPinned, v1.ReasonSkillUnpinnedRolling,
			"skill "+spec.CanonicalName+" tracks a mutable ref; pin to a SHA for reproducibility/security")
	}
}
