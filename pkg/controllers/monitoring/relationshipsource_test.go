// pkg/controllers/monitoring/relationshipsource_test.go
package monitoring

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

func relationshipSourceTarget(t *testing.T) Target {
	t.Helper()
	for _, tg := range Targets() {
		if tg.GVKName == "RelationshipSource" {
			return tg
		}
	}
	t.Fatal("RelationshipSource target missing: directory-sync failures reach nobody without it")
	return Target{}
}

// srcWithConditions returns a RelationshipSource carrying exactly conds.
func srcWithConditions(conds ...metav1.Condition) *spiceboxv1alpha1.RelationshipSource {
	src := &spiceboxv1alpha1.RelationshipSource{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "demo-directory"},
		Spec:       spiceboxv1alpha1.RelationshipSourceSpec{Kind: "github"},
	}
	src.Status.Conditions = conds
	return src
}

func newSourceReconciler(t *testing.T, src *spiceboxv1alpha1.RelationshipSource) (*Reconciler, *recorder, client.Client) {
	t.Helper()
	scheme := testfixtures.NewScheme(t)
	rec := &recorder{}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(src).
		WithStatusSubresource(&spiceboxv1alpha1.RelationshipSource{}).Build()
	return &Reconciler{Client: c, Publish: rec.publish, Target: relationshipSourceTarget(t), tracker: newTracker()}, rec, c
}

func reconcileSource(t *testing.T, r *Reconciler) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKey{Namespace: "default", Name: "demo-directory"},
	})
	require.NoError(t, err)
}

func setSourceConditions(t *testing.T, c client.Client, conds ...metav1.Condition) {
	t.Helper()
	var cur spiceboxv1alpha1.RelationshipSource
	require.NoError(t, c.Get(context.Background(),
		client.ObjectKey{Namespace: "default", Name: "demo-directory"}, &cur))
	cur.Status.Conditions = conds
	require.NoError(t, c.Status().Update(context.Background(), &cur))
}

func partialTrue(msg string) metav1.Condition {
	return metav1.Condition{
		Type:    spiceboxv1alpha1.RelationshipSourceConditionPartialFailure,
		Status:  metav1.ConditionTrue,
		Reason:  spiceboxv1alpha1.ReasonRelationshipSourceScopeErrors,
		Message: msg,
	}
}

func partialFalse() metav1.Condition {
	return metav1.Condition{
		Type:   spiceboxv1alpha1.RelationshipSourceConditionPartialFailure,
		Status: metav1.ConditionFalse,
		Reason: spiceboxv1alpha1.ReasonRelationshipSourceAllScopesSynced,
	}
}

func readyTrue() metav1.Condition {
	return metav1.Condition{
		Type:   spiceboxv1alpha1.RelationshipSourceConditionReady,
		Status: metav1.ConditionTrue,
		Reason: spiceboxv1alpha1.ReasonRelationshipSourceSynced,
	}
}

// The gap this row closes: a source reporting Ready=True/Synced whose every
// per-scope fetch failed used to reach nobody at all. Ready is True the whole
// time here — that is deliberate, and it is exactly why the second rule has to
// exist.
func TestRelationshipSourcePartialFailureRule_EmitsWhileReadyStaysTrue(t *testing.T) {
	msg := "156 scope(s) failed this pass; the pass was otherwise applied"
	r, rec, c := newSourceReconciler(t, srcWithConditions(readyTrue(), partialTrue(msg)))

	reconcileSource(t, r)

	events := rec.snapshot()
	require.Len(t, events, 1, "a partially-failed sync must emit exactly one event")
	assert.Equal(t, spiceboxv1alpha1.RelationshipSourceConditionPartialFailure, events[0].Condition,
		"Ready is True, so this can only have come from the PartialFailure rule")
	assert.Equal(t, channelevents.MonitoringTransitionFailed, events[0].Transition)
	assert.Equal(t, channelevents.MonitoringLevelWarning, events[0].Level,
		"per-scope failures are non-fatal; the rest of the directory did sync")
	assert.Equal(t, "reconcile", events[0].Category)
	assert.Equal(t, msg, events[0].Summary, "the count must ride along, or the alert says nothing actionable")
	assert.Contains(t, events[0].Hint, "demo-directory", "{name} must be substituted")

	// The repair: a clean pass flips the condition False.
	setSourceConditions(t, c, readyTrue(), partialFalse())
	reconcileSource(t, r)

	events = rec.snapshot()
	require.Len(t, events, 2, "the False flip must emit the matching recovery")
	assert.Equal(t, channelevents.MonitoringTransitionRecovered, events[1].Transition)
	assert.Equal(t, channelevents.MonitoringLevelWarning, events[1].Level,
		"recovery carries the failure's level so one filter surfaces both")
}

// The hard-failure rule: a source that cannot sync at all.
func TestRelationshipSourceReadyRule_EmitsOnHardFailure(t *testing.T) {
	r, rec, _ := newSourceReconciler(t, srcWithConditions(metav1.Condition{
		Type:    spiceboxv1alpha1.RelationshipSourceConditionReady,
		Status:  metav1.ConditionFalse,
		Reason:  spiceboxv1alpha1.ReasonRelationshipSourceAuthResolveFailed,
		Message: "resolve credential \"forge-pat\": secret not found",
	}))

	reconcileSource(t, r)

	events := rec.snapshot()
	require.Len(t, events, 1)
	assert.Equal(t, spiceboxv1alpha1.RelationshipSourceConditionReady, events[0].Condition)
	assert.Equal(t, channelevents.MonitoringLevelError, events[0].Level,
		"a directory that cannot sync at all leaves every downstream decision on stale membership")
	assert.Equal(t, spiceboxv1alpha1.ReasonRelationshipSourceAuthResolveFailed, events[0].Reason)
}

// A source that has never completed a pass carries neither condition, and must
// be silent: absence of evidence is not a failure. Guards against a future
// BadStatus/polarity edit turning "unstamped" into an alert on every source in
// the cluster at once.
func TestRelationshipSourceRules_AbsentConditionsAreSilent(t *testing.T) {
	r, rec, _ := newSourceReconciler(t, srcWithConditions())
	reconcileSource(t, r)
	assert.Empty(t, rec.snapshot(), "no conditions ⇒ no event")
}

// A clean source must be silent too — the PartialFailure condition is PRESENT
// and False on every healthy pass, so a rule reading its presence rather than
// its status would alert on every working directory in the cluster.
func TestRelationshipSourceRules_CleanSourceIsSilent(t *testing.T) {
	r, rec, _ := newSourceReconciler(t, srcWithConditions(readyTrue(), partialFalse()))
	reconcileSource(t, r)
	assert.Empty(t, rec.snapshot(), "Ready=True + PartialFailure=False is a healthy source")
}

// Terminal suppresses the cold-start re-announce, and is correct only for a
// condition that records a PAST incident and never recovers (Rule.Terminal's
// own doc). Both of these clear themselves the moment the directory is
// repaired — Ready flips True on the next good pass, PartialFailure is
// rewritten from every completed pass — so a source still broken across an
// operator restart must re-surface rather than be swallowed.
func TestRelationshipSourceRules_AreNotTerminal(t *testing.T) {
	tgt := relationshipSourceTarget(t)
	require.Len(t, tgt.Rules, 2, "hard failure and partial failure are separate facts and need separate rules")

	byType := map[string]Rule{}
	for _, rule := range tgt.Rules {
		assert.False(t, rule.Terminal,
			"%s recovers on its own; Terminal would hide a source still broken after a restart", rule.ConditionType)
		byType[rule.ConditionType] = rule
	}

	ready, ok := byType[spiceboxv1alpha1.RelationshipSourceConditionReady]
	require.True(t, ok, "the hard-failure rule must be present")
	assert.Equal(t, metav1.ConditionFalse, ready.BadStatus, "Ready is True-is-healthy")

	partial, ok := byType[spiceboxv1alpha1.RelationshipSourceConditionPartialFailure]
	require.True(t, ok, "the partial-failure rule must be present")
	assert.Equal(t, metav1.ConditionTrue, partial.BadStatus,
		"PartialFailure is True-is-BAD; reading it as False-is-bad inverts it into an alert on every healthy source")
}
