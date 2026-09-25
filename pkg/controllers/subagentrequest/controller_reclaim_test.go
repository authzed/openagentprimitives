package subagentrequest

// Reclamation of a RESOLVED delegation under a still-live parent.
//
// The owner-reference cascade only ever runs from the parent's own deletion, so
// a long-lived conversational parent — the shape `chat` mode exists for —
// accrues one request, one Channel and one terminal child per delegation for
// its entire lifetime unless something reclaims them on a clock. These tests
// pin that clock: when it starts, that it is not zero, and that the delete it
// eventually performs is the request itself, which is what makes the cascade
// take the child and their Channel with it.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// resolvedFixture seeds a delegation already in a terminal phase, with its
// completion clock set to `completedAt` (nil for a request resolved before the
// field existed), plus the child and Channel a conversational delegation
// leaves behind. The reconciler's clock starts at `now`.
func resolvedFixture(
	t *testing.T, phase string, completedAt *metav1.Time, now time.Time,
) (*Reconciler, client.Client) {
	t.Helper()
	sr := requestWithChild(t, "req1", "demo-parent", "demo-coder", "req1-child")
	sr.Spec.Mode = v1.SubagentModeChat
	sr.Status.Phase = phase
	sr.Status.CompletionTime = completedAt
	child := childSession(t, "req1-child", "demo-coder", v1.AgentSessionStatus{
		Phase: v1.AgentSessionPhaseSucceeded,
	})
	ch := &v1.Channel{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "req1-inbox"}}
	r, c := newReconciler(t,
		conversationalParentClass(t),
		childClass(t, "demo-coder"),
		parentSession(t, "demo-parent", "demo-lead"),
		sr, child, ch,
	)
	clk := &fixedClock{t: now}
	r.Now = clk.now
	return r, c
}

func requestExists(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	var got v1.SubagentRequest
	err := c.Get(t.Context(), types.NamespacedName{Namespace: "ns", Name: name}, &got)
	if apierrors.IsNotFound(err) {
		return false
	}
	require.NoError(t, err, "get request %q", name)
	return true
}

// Reaching a terminal phase must stamp the clock in the SAME status update, and
// must schedule the reconcile that will do the reclaiming: nothing else
// revisits a resolved request, because the controller's only Watch is on
// AgentSession and a finished child stops producing events.
func TestResolve_TerminalPhaseStampsTheClockAndSchedulesTheReclaim(t *testing.T) {
	cases := []struct {
		name       string
		childPhase string
		wantPhase  string
	}{
		{
			name:       "child succeeded: Succeeded, clocked, and requeued for retention",
			childPhase: v1.AgentSessionPhaseSucceeded,
			wantPhase:  v1.SubagentRequestPhaseSucceeded,
		},
		{
			name:       "child failed: Failed, clocked, and requeued for retention",
			childPhase: v1.AgentSessionPhaseFailed,
			wantPhase:  v1.SubagentRequestPhaseFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sr := requestWithChild(t, "req1", "demo-parent", "demo-coder", "req1-child")
			child := childSession(t, "req1-child", "demo-coder", v1.AgentSessionStatus{Phase: tc.childPhase})
			r, c := newReconciler(t,
				conversationalParentClass(t), childClass(t, "demo-coder"),
				parentSession(t, "demo-parent", "demo-lead"), sr, child,
			)
			clk := &fixedClock{t: time.Now()}
			r.Now = clk.now

			res := reconcileOnce(t, r, "req1")

			got := getRequest(t, c, "req1")
			assert.Equal(t, tc.wantPhase, got.Status.Phase)
			require.NotNil(t, got.Status.CompletionTime,
				"a terminal phase without a completion clock is a delegation nothing ever reclaims")
			assert.WithinDuration(t, clk.t, got.Status.CompletionTime.Time, time.Second)
			assert.Equal(t, DefaultTerminalRetention, res.RequeueAfter,
				"the requeue IS the reclaim schedule; without it nothing revisits a resolved request")
		})
	}
}

// A policy refusal never creates a child, and is reclaimed on the same clock as
// any other terminal phase — deny() must not be the one terminal writer that
// forgets to stamp it.
func TestDeny_StampsTheClockLikeEveryOtherTerminalPhase(t *testing.T) {
	r, c := newReconciler(t,
		parentClass(t, "demo-lead", "demo-coder"), // roster: coder only
		childClass(t, "demo-sre"),
		parentSession(t, "demo-parent", "demo-lead"),
		request(t, "req1", "demo-parent", "demo-sre"), // off-roster
	)
	clk := &fixedClock{t: time.Now()}
	r.Now = clk.now

	res := reconcileOnce(t, r, "req1")

	got := getRequest(t, c, "req1")
	require.Equal(t, v1.SubagentRequestPhaseDenied, got.Status.Phase, "fixture must actually be denied")
	require.NotNil(t, got.Status.CompletionTime, "a denial is resolved, so it is reclaimed on the same clock")
	assert.Equal(t, DefaultTerminalRetention, res.RequeueAfter)
}

// Within the retention window the request survives, and the requeue counts down
// rather than restarting: a resolved delegation must not be kept indefinitely
// by reconciles that keep resetting its own deadline.
func TestReclaimTerminal_WithinRetention_KeptAndTheRequeueCountsDown(t *testing.T) {
	completed := metav1.NewTime(time.Now())
	r, c := resolvedFixture(t, v1.SubagentRequestPhaseSucceeded, &completed,
		completed.Time.Add(DefaultTerminalRetention/4))

	res := reconcileOnce(t, r, "req1")

	assert.True(t, requestExists(t, c, "req1"),
		"a delegation resolved a moment ago is still the record of what was delegated")
	assert.InDelta(t, float64(DefaultTerminalRetention*3/4), float64(res.RequeueAfter), float64(time.Second),
		"the requeue must be the REMAINDER of the window, not a fresh one")
}

// Past the window the request is deleted. That single delete is the whole
// mechanism: the child session and the per-request `agent` Channel are
// owner-ref'd to the request, so Kubernetes garbage collection reclaims them
// without this controller naming either — which is why the assertion is on the
// request and on the reconcile going quiet, not on the child being gone (the
// fake client runs no GC).
func TestReclaimTerminal_PastRetention_TheRequestIsDeletedAndTheReconcileGoesQuiet(t *testing.T) {
	completed := metav1.NewTime(time.Now())
	r, c := resolvedFixture(t, v1.SubagentRequestPhaseSucceeded, &completed,
		completed.Time.Add(DefaultTerminalRetention+time.Minute))

	res := reconcileOnce(t, r, "req1")

	assert.False(t, requestExists(t, c, "req1"), "a delegation kept for its full retention must be reclaimed")
	assert.Zero(t, res.RequeueAfter, "there is nothing left to revisit")

	// And a reconcile of the now-missing request is a clean no-op rather than
	// an error, since the informer may still deliver an event for it.
	assert.Zero(t, reconcileOnce(t, r, "req1").RequeueAfter)
}

// A request resolved before status.completionTime existed carries no clock.
// Reading "no clock" as "expired" would make an operator upgrade sweep away
// every historical delegation in the cluster on its first reconcile pass, so
// the clock is STARTED instead and the request keeps its full window.
func TestReclaimTerminal_NoClockOnAnUpgradedRequest_StartsItRatherThanReclaiming(t *testing.T) {
	now := time.Now()
	r, c := resolvedFixture(t, v1.SubagentRequestPhaseSucceeded, nil, now)

	res := reconcileOnce(t, r, "req1")

	require.True(t, requestExists(t, c, "req1"),
		"an upgrade must not reclaim a delegation it has no clock for")
	got := getRequest(t, c, "req1")
	require.NotNil(t, got.Status.CompletionTime, "the clock must be started")
	assert.WithinDuration(t, now, got.Status.CompletionTime.Time, time.Second)
	assert.Equal(t, DefaultTerminalRetention, res.RequeueAfter, "and the full window granted from here")
}

// The configured retention wins over the default, and an unset one falls back
// to the default rather than to zero. Zero must never read as "reclaim
// immediately": that is the un-wired reconciler — a test, or a binary that
// forgot the field — and it would delete every delegation the instant it
// finished.
func TestTerminalRetention_ZeroIsUnsetNotImmediate(t *testing.T) {
	cases := []struct {
		name       string
		configured time.Duration
		want       time.Duration
	}{
		{name: "unset: the built-in default, never zero", configured: 0, want: DefaultTerminalRetention},
		{name: "configured: the configured value wins", configured: 5 * time.Minute, want: 5 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Reconciler{TerminalRetention: tc.configured}
			assert.Equal(t, tc.want, r.terminalRetention())
		})
	}

	// And the behavioural half: an un-wired reconciler keeps a delegation that
	// resolved this instant.
	completed := metav1.NewTime(time.Now())
	r, c := resolvedFixture(t, v1.SubagentRequestPhaseSucceeded, &completed, completed.Time)
	r.TerminalRetention = 0

	reconcileOnce(t, r, "req1")
	assert.True(t, requestExists(t, c, "req1"),
		"a zero retention is an unset one; it must not reclaim a delegation the moment it finishes")
}

// An AwaitingParent request is NOT terminal, so the reclaim pass must not
// touch it: the child is parked on an answer that may still arrive, and what
// bounds it is the per-exchange parent-reply timeout, not this clock.
func TestReclaimTerminal_AwaitingParentIsNotReclaimed(t *testing.T) {
	// A clock stamped a day ago, which the reclaim pass would treat as long
	// expired if it read the phase as terminal — and a LIVE child still holding
	// its question, so the pass under test is the awaiting one.
	long := metav1.NewTime(time.Now().Add(-24 * time.Hour))
	sr := requestWithChild(t, "req1", "demo-parent", "demo-coder", "req1-child")
	sr.Spec.Mode = v1.SubagentModeChat
	sr.Status.Phase = v1.SubagentRequestPhaseAwaitingParent
	sr.Status.CompletionTime = &long
	sr.Status.Exchange = 1
	child := childSession(t, "req1-child", "demo-coder", v1.AgentSessionStatus{
		Phase:          v1.AgentSessionPhaseRunning,
		ParentExchange: &v1.ParentExchange{Exchange: 1, Pending: true, Question: "which repo?"},
	})
	r, c := newReconciler(t,
		conversationalParentClass(t), childClass(t, "demo-coder"),
		parentSession(t, "demo-parent", "demo-lead"), sr, child,
	)
	clk := &fixedClock{t: time.Now()}
	r.Now = clk.now

	reconcileOnce(t, r, "req1")

	assert.True(t, requestExists(t, c, "req1"),
		"a parked delegation is live work, however long ago its clock was stamped")
	assert.Equal(t, v1.SubagentRequestPhaseAwaitingParent, getRequest(t, c, "req1").Status.Phase)
}
