package spiceboxsession

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
)

func TestApplySandboxStatus(t *testing.T) {
	cases := []struct {
		name        string
		st          sandboxkinds.Status
		wantType    string
		wantStatus  metav1.ConditionStatus
		wantReason  string
		wantMessage string
	}{
		{
			name:       "Ready: Ready=True carrying the backend's reason",
			st:         sandboxkinds.Status{Phase: sandboxkinds.PhaseReady, Reason: sandboxkinds.ReasonReady},
			wantType:   spiceboxv1alpha1.SpiceboxSessionConditionReady,
			wantStatus: metav1.ConditionTrue,
			wantReason: sandboxkinds.ReasonReady,
		},
		{
			name:        "Failed: Failed=True, message preserved for the user",
			st:          sandboxkinds.Status{Phase: sandboxkinds.PhaseFailed, Reason: sandboxkinds.ReasonOOMKilled, Message: "killed at 256Mi"},
			wantType:    spiceboxv1alpha1.SpiceboxSessionConditionFailed,
			wantStatus:  metav1.ConditionTrue,
			wantReason:  sandboxkinds.ReasonOOMKilled,
			wantMessage: "killed at 256Mi",
		},
		{
			// Pending sets Ready=False and leaves Progressing ALONE. The create
			// path owns Progressing via setProgressing, and an observed phase
			// overwriting it would replace that path's specific reason (e.g.
			// waiting on the workspace claim) with a generic one.
			name:       "Pending: Ready=False, Progressing untouched",
			st:         sandboxkinds.Status{Phase: sandboxkinds.PhasePending, Reason: sandboxkinds.ReasonNotReady},
			wantType:   spiceboxv1alpha1.SpiceboxSessionConditionReady,
			wantStatus: metav1.ConditionFalse,
			wantReason: sandboxkinds.ReasonNotReady,
		},
		{
			// A backend may report any reason it likes; the controller must carry
			// it through rather than recognising only the standard set.
			name:       "a custom backend reason is carried through verbatim",
			st:         sandboxkinds.Status{Phase: sandboxkinds.PhaseFailed, Reason: "VendorQuotaExceeded", Message: "account limit"},
			wantType:   spiceboxv1alpha1.SpiceboxSessionConditionFailed,
			wantStatus: metav1.ConditionTrue,
			wantReason: "VendorQuotaExceeded",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := &spiceboxv1alpha1.SpiceboxSession{}
			applySandboxStatus(sess, tc.st)

			c := meta.FindStatusCondition(sess.Status.Conditions, tc.wantType)
			require.NotNil(t, c, "expected a %s condition", tc.wantType)
			assert.Equal(t, tc.wantStatus, c.Status)
			assert.Equal(t, tc.wantReason, c.Reason)
			if tc.wantMessage != "" {
				assert.Equal(t, tc.wantMessage, c.Message)
			}
		})
	}
}

// TestClearStaleSandboxFailure covers both silent failure directions: clearing
// too narrowly leaves a recovered bundle permanently Failed (the crashed- and
// start-failed-then-recovered cases, plus a custom backend reason no constant
// enumerates), and clearing too broadly erases a class-validation failure that
// must survive until its own clear path runs.
func TestClearStaleSandboxFailure(t *testing.T) {
	cases := []struct {
		name      string
		reason    string
		wantClear bool
	}{
		// Guards against clearing too narrowly: a crashed sandbox is replaced
		// and the replacement recovers.
		{name: "a crashed-then-recovered sandbox is cleared", reason: sandboxkinds.ReasonCrashed, wantClear: true},
		{name: "an OOM-then-recovered sandbox is cleared", reason: sandboxkinds.ReasonOOMKilled, wantClear: true},
		// Guards against clearing too narrowly: a sandbox that never started is
		// replaced and the replacement recovers.
		{name: "a start-failed-then-recovered sandbox is cleared", reason: sandboxkinds.ReasonStartFailed, wantClear: true},
		{
			// The whole reason for inverting the test: a backend may report a
			// reason nobody enumerated, and a recovered bundle must still recover.
			name: "a custom backend reason is cleared", reason: "VendorQuotaExceeded", wantClear: true,
		},
		{
			name:   "ClassMissing is NOT cleared: it has its own clear path",
			reason: spiceboxv1alpha1.ReasonClassMissing, wantClear: false,
		},
		{
			name:   "ClassInvalid is NOT cleared: it has its own clear path",
			reason: spiceboxv1alpha1.ReasonClassInvalid, wantClear: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := &spiceboxv1alpha1.SpiceboxSession{}
			meta.SetStatusCondition(&sess.Status.Conditions, metav1.Condition{
				Type:   spiceboxv1alpha1.SpiceboxSessionConditionFailed,
				Status: metav1.ConditionTrue, Reason: tc.reason, Message: "prior failure",
			})

			clearStaleSandboxFailure(sess)

			got := meta.FindStatusCondition(sess.Status.Conditions, spiceboxv1alpha1.SpiceboxSessionConditionFailed)
			if tc.wantClear {
				assert.Nil(t, got, "a sandbox-lifecycle failure must not outlive a healthy replacement")
				return
			}
			require.NotNil(t, got, "a class-validation failure must survive")
			assert.Equal(t, tc.reason, got.Reason)
		})
	}
}

// A Failed condition that is already False is left alone — there is nothing
// stale to clear, and rewriting it would churn LastTransitionTime.
func TestClearStaleSandboxFailure_IgnoresAFalseCondition(t *testing.T) {
	sess := &spiceboxv1alpha1.SpiceboxSession{}
	meta.SetStatusCondition(&sess.Status.Conditions, metav1.Condition{
		Type:   spiceboxv1alpha1.SpiceboxSessionConditionFailed,
		Status: metav1.ConditionFalse, Reason: "ClassAppeared",
	})

	clearStaleSandboxFailure(sess)

	got := meta.FindStatusCondition(sess.Status.Conditions, spiceboxv1alpha1.SpiceboxSessionConditionFailed)
	require.NotNil(t, got)
	assert.Equal(t, metav1.ConditionFalse, got.Status)
	assert.Equal(t, "ClassAppeared", got.Reason)
}
