// Package projectors holds the per-CRD config.Projector implementations that
// sit behind the admin UI's Config phase. Each file registers one projector
// via init() -> config.Register; the package is blank-imported by admind so
// the registrations run. Every projector flattens its CRD's CRs into the
// presentation-agnostic config.ResourceRow shape.
package projectors

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// projectStatus maps a CRD's status conditions onto a ResourceRow's short
// Status word + machine StatusReason.
//
//   - primary condition True  -> okWord ("Valid"/"Ready"/"Connected"), reason
//     carried from the condition.
//   - primary condition False -> "Degraded", reason = condition Reason (or
//     Message when Reason is empty).
//   - primary condition ABSENT -> "Unknown" — an unstamped resource must never
//     read as healthy, so we surface the missing-signal explicitly rather than
//     defaulting to the ok word.
func projectStatus(conds []metav1.Condition, condType, okWord string) (status, reason string) {
	c := meta.FindStatusCondition(conds, condType)
	if c == nil {
		return "Unknown", ""
	}
	switch c.Status {
	case metav1.ConditionTrue:
		return okWord, reasonOrMessage(c)
	case metav1.ConditionFalse:
		return "Degraded", reasonOrMessage(c)
	default:
		return "Unknown", reasonOrMessage(c)
	}
}

// reasonOrMessage prefers the machine Reason, falling back to the human
// Message so a condition that set only one of the two still carries a note.
func reasonOrMessage(c *metav1.Condition) string {
	if c.Reason != "" {
		return c.Reason
	}
	return c.Message
}

// projectSidecarToolboxStatus folds a SidecarToolbox's Valid + Reachable
// conditions into ONE display status word, shared by the tools list row and the
// tool detail so the two never drift. A Valid failure dominates (an invalid CR
// is unusable). Otherwise reachability is surfaced distinctly:
//
//   - Reachable=Unknown/DeferredToSession -> "Deferred": the operator
//     intentionally does not probe a secret-gated sidecar; reachability is
//     verified per-session by the runner. The frontend renders "Deferred" in its
//     own tone (primary/blue), so it never reads as a failure (Degraded/amber)
//     nor as unknown/broken (grey) nor as healthy (Valid/green).
//   - Reachable=False -> "Degraded": a real in-pod probe failure.
//   - otherwise -> the Valid word (Valid/green).
func projectSidecarToolboxStatus(conds []metav1.Condition) (status, reason string) {
	valid, vreason := projectStatus(conds, spiceboxv1alpha1.SidecarToolboxConditionValid, "Valid")
	if valid != "Valid" {
		// Not-Valid (Degraded/Unknown) dominates the display.
		return valid, vreason
	}
	reach := meta.FindStatusCondition(conds, spiceboxv1alpha1.SidecarToolboxConditionReachable)
	if reach != nil {
		switch {
		case reach.Status == metav1.ConditionUnknown && reach.Reason == spiceboxv1alpha1.ReasonSidecarToolboxDeferredToSession:
			return "Deferred", reasonOrMessage(reach)
		case reach.Status == metav1.ConditionFalse:
			return "Degraded", reasonOrMessage(reach)
		}
	}
	return valid, vreason
}

// relationshipSourcePartialFailure returns the PartialFailure condition when a
// directory sync's last COMPLETED pass had per-scope errors, and nil
// otherwise (including when the condition is absent — a source that has never
// completed a pass has no verdict to report).
//
// Read through a helper rather than inline at the two call sites because the
// polarity is the unusual one: True is BAD here, the way
// AgentSessionConditionFailed's is, not the way Ready's is.
func relationshipSourcePartialFailure(conds []metav1.Condition) *metav1.Condition {
	c := meta.FindStatusCondition(conds, spiceboxv1alpha1.RelationshipSourceConditionPartialFailure)
	if c == nil || c.Status != metav1.ConditionTrue {
		return nil
	}
	return c
}

// projectRelationshipSourceStatus folds a RelationshipSource's Ready +
// PartialFailure conditions into ONE display word, shared by the directory
// list row and the directory detail so the two never drift — the same job
// projectSidecarToolboxStatus does for Valid + Reachable.
//
// A Ready failure dominates: a source that cannot sync at all is a strictly
// worse state than one that synced most of a directory. Otherwise a partial
// failure downgrades the row from Ready to Degraded, which is the entire point
// of the condition: a GitHub sync whose every per-repository team fetch
// answered 403 reported Ready=True/Synced with 156 scopes processed, and the
// console showed a healthy source. Ready's own meaning is unchanged — this
// reads a SECOND condition rather than redefining the first.
func projectRelationshipSourceStatus(conds []metav1.Condition) (status, reason string) {
	ready, rreason := projectStatus(conds, spiceboxv1alpha1.RelationshipSourceConditionReady, "Ready")
	if ready != "Ready" {
		return ready, rreason
	}
	if pf := relationshipSourcePartialFailure(conds); pf != nil {
		return "Degraded", reasonOrMessage(pf)
	}
	return ready, rreason
}

// editCmd builds the `kubectl edit` ManageCmd string. ns == "" yields the
// cluster-scoped form (no -n flag). The admin UI is read-only — it shows the
// command an operator runs, it does not run it.
func editCmd(kind, name, ns string) string {
	if ns == "" {
		return fmt.Sprintf("kubectl edit %s %s", kind, name)
	}
	return fmt.Sprintf("kubectl edit %s %s -n %s", kind, name, ns)
}

// shortSHA truncates a commit SHA to 12 chars for compact display.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
