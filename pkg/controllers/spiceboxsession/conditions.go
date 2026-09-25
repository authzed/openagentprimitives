package spiceboxsession

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
)

// applySandboxStatus folds a backend's status onto the session's conditions.
//
// Phase decides which condition moves; Reason and Message are passed through
// verbatim, including a reason no standard constant covers — a backend may
// report whatever describes its own failure, and flattening that would destroy
// the detail an operator needs.
func applySandboxStatus(sess *spiceboxv1alpha1.SpiceboxSession, st sandboxkinds.Status) {
	switch st.Phase {
	case sandboxkinds.PhaseFailed:
		// A terminal failure clears Ready and Progressing outright rather than
		// setting them False: the session is not "not ready yet", it is done.
		conditions.Set(sess, &sess.Status.Conditions, metav1.Condition{
			Type: spiceboxv1alpha1.SpiceboxSessionConditionFailed, Status: metav1.ConditionTrue,
			Reason: st.Reason, Message: st.Message,
		})
		meta.RemoveStatusCondition(&sess.Status.Conditions, spiceboxv1alpha1.SpiceboxSessionConditionReady)
		meta.RemoveStatusCondition(&sess.Status.Conditions, spiceboxv1alpha1.SpiceboxSessionConditionProgressing)

	case sandboxkinds.PhaseReady:
		conditions.Set(sess, &sess.Status.Conditions, metav1.Condition{
			Type: spiceboxv1alpha1.SpiceboxSessionConditionReady, Status: metav1.ConditionTrue,
			Reason: st.Reason, Message: "sandbox is ready",
		})
		conditions.SetFalse(sess, &sess.Status.Conditions,
			spiceboxv1alpha1.SpiceboxSessionConditionProgressing, st.Reason, "")

	default:
		// Pending and Gone both mean "not serving". Progressing is deliberately
		// NOT touched here: the create path owns it via setProgressing, and
		// overwriting it from an observed phase would clobber the more specific
		// reason that path just set (e.g. waiting on the workspace claim).
		conditions.SetFalse(sess, &sess.Status.Conditions,
			spiceboxv1alpha1.SpiceboxSessionConditionReady, st.Reason, st.Message)
	}
}

// clearStaleSandboxFailure drops a Failed condition left by a previous sandbox
// once a healthy one is observed, so a bundle that crashed and was replaced
// stops reporting BundleFailed to its AgentSession.
//
// The exclusion list is what is enumerable. Class-validation failures are the
// only Failed conditions this controller sets itself, and they have their own
// clear path when the class resolves; they never coexist with a live sandbox.
// Everything else came from the sandbox lifecycle, where a backend may report
// any reason it likes — so enumerating the CLEARABLE set would silently stop
// matching a backend that reported something unanticipated, and the bundle
// would look permanently failed.
func clearStaleSandboxFailure(sess *spiceboxv1alpha1.SpiceboxSession) {
	f := meta.FindStatusCondition(sess.Status.Conditions, spiceboxv1alpha1.SpiceboxSessionConditionFailed)
	if f == nil || f.Status != metav1.ConditionTrue {
		return
	}
	if f.Reason == spiceboxv1alpha1.ReasonClassMissing || f.Reason == spiceboxv1alpha1.ReasonClassInvalid {
		return
	}
	meta.RemoveStatusCondition(&sess.Status.Conditions, spiceboxv1alpha1.SpiceboxSessionConditionFailed)
}
