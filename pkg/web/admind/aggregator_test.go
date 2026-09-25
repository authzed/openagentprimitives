package admind_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/web/admind"
)

func sessionCR(ns, name, class, phase string) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{}
	s.Namespace, s.Name = ns, name
	s.Spec.Class = class
	s.Status.Phase = phase
	s.Status.StartedAt = &metav1.Time{Time: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	s.Status.Progress = &spiceboxv1alpha1.AgentSessionProgress{TurnCount: 2, InputTokens: 1000, OutputTokens: 200, ToolCallCount: 3}
	return s
}

func envelopeBytes(t *testing.T, ns, name string, kind channelevents.Kind, payload any) []byte {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	env := channelevents.Envelope{Version: 1, Kind: kind,
		Session: channelevents.SessionRef{Namespace: ns, Name: name}, Payload: raw}
	b, err := json.Marshal(env)
	require.NoError(t, err)
	return b
}

// outSubject is the subject ns/name's own runner is authorized to publish on
// ("ap.session.<ns>.<name>.out.<kind>"), which is what the aggregator keys off.
func outSubject(ns, name string, kind channelevents.Kind) string {
	return channelevents.SubjectOut(channelevents.SubjectPrefix(ns, name), kind)
}

// fold delivers an envelope for ns/name on ns/name's OWN out subject — the
// legitimate case, where the authorized subject and the envelope body agree.
// A test that needs them to DISAGREE (the impersonation attack) calls
// HandleEnvelopeBytes directly with a mismatched pair.
func fold(t *testing.T, ag *admind.Aggregator, ns, name string, kind channelevents.Kind, payload any) {
	t.Helper()
	ag.HandleEnvelopeBytes(outSubject(ns, name, kind), envelopeBytes(t, ns, name, kind, payload))
}

func TestAggregator_SessionLifecycleAndOverlay(t *testing.T) {
	ag := admind.NewAggregator(testr.New(t))

	ag.UpsertSession(sessionCR("default", "s1", "support-bot", "Running"))

	snap := ag.Snapshot()
	require.Len(t, snap, 1)
	assert.Equal(t, "support-bot", snap[0].Class)
	assert.Equal(t, "Running", snap[0].Phase)
	assert.Equal(t, int64(1000), snap[0].InputTokens)

	// NATS overlay: progress + activity + status text.
	fold(t, ag, "default", "s1", channelevents.KindTurnProgress,
		channelevents.TurnProgressPayload{InputTokens: 5000, OutputTokens: 900, ElapsedSeconds: 42, Seq: 7})
	fold(t, ag, "default", "s1", channelevents.KindTurnActivity,
		channelevents.TurnActivityPayload{Active: true})
	fold(t, ag, "default", "s1", channelevents.KindNotification,
		channelevents.NotificationPayload{Text: "Running terraform plan"})

	st, recent, ok := ag.Get("default", "s1")
	require.True(t, ok)
	assert.Equal(t, int64(5000), st.InputTokens, "NATS progress overrides CRD snapshot")
	assert.Equal(t, 42, st.ElapsedSeconds)
	assert.True(t, st.Active)
	assert.Equal(t, "Running terraform plan", st.StatusText)
	assert.Len(t, recent, 3, "ring buffer captured the envelopes")

	// Overlay-no-regress: a stale CRD re-upsert (InputTokens=1000) must not
	// overwrite the NATS overlay (InputTokens=5000).
	ag.UpsertSession(sessionCR("default", "s1", "support-bot", "Running"))
	st, _, ok = ag.Get("default", "s1")
	require.True(t, ok)
	assert.Equal(t, int64(5000), st.InputTokens, "stale CRD re-upsert must not regress NATS overlay")

	// Unknown sessions are created lazily from envelopes (watch can lag NATS).
	fold(t, ag, "default", "s9", channelevents.KindTurnActivity,
		channelevents.TurnActivityPayload{Active: true})
	_, _, ok = ag.Get("default", "s9")
	assert.True(t, ok)

	ag.DeleteSession("default", "s1")
	_, _, ok = ag.Get("default", "s1")
	assert.False(t, ok)
	assert.Len(t, ag.Snapshot(), 1)
}

// TestAggregator_TerminalCRDPhaseClearsActivityOverlay proves the CRD is
// authoritative for "definitely not working". A runner that is OOM-killed,
// evicted, or crashes mid-turn never publishes turn_activity(active:false) —
// it is the only publisher of that kind — so without a CRD-side backstop the
// overlay latches true for as long as the CR exists, permanently inflating the
// ActiveSessions KPI and painting a dead session with a live pulsing dot.
func TestAggregator_TerminalCRDPhaseClearsActivityOverlay(t *testing.T) {
	ag := admind.NewAggregator(testr.New(t))
	ag.UpsertSession(sessionCR("default", "s1", "support-bot", spiceboxv1alpha1.AgentSessionPhaseRunning))
	fold(t, ag, "default", "s1", channelevents.KindTurnActivity,
		channelevents.TurnActivityPayload{Active: true})

	st, _, ok := ag.Get("default", "s1")
	require.True(t, ok)
	require.True(t, st.Active, "precondition: the NATS overlay armed the working dot")

	// The runner dies mid-turn; the operator moves the CR to Failed. No
	// active:false ever arrives.
	ag.UpsertSession(sessionCR("default", "s1", "support-bot", spiceboxv1alpha1.AgentSessionPhaseFailed))

	st, _, ok = ag.Get("default", "s1")
	require.True(t, ok)
	assert.False(t, st.Active, "a terminal CRD phase must clear the latched NATS activity overlay")
	assert.Equal(t, channelevents.PauseCauseFailed, st.ActivityCause, "the cleared overlay carries the phase's pause cause")
}

// TestAggregator_LateActivityCannotRearmAParkedSession closes the write-side
// half of the same defect: a runner inside its termination grace period keeps
// publishing, and its turn_activity(active:true) must not re-arm the dot for a
// session the CRD has already reported as finished.
func TestAggregator_LateActivityCannotRearmAParkedSession(t *testing.T) {
	ag := admind.NewAggregator(testr.New(t))
	ag.UpsertSession(sessionCR("default", "s1", "support-bot", spiceboxv1alpha1.AgentSessionPhaseSucceeded))

	fold(t, ag, "default", "s1", channelevents.KindTurnActivity,
		channelevents.TurnActivityPayload{Active: true})

	st, _, ok := ag.Get("default", "s1")
	require.True(t, ok)
	assert.False(t, st.Active, "a late activity signal must not re-arm a session the CRD calls finished")
	assert.Equal(t, channelevents.PauseCauseComplete, st.ActivityCause)
}

// TestAggregator_SeedsActivityFromCRDPhaseUntilAnOverlayArrives covers the
// restart half: a fresh Aggregator is exactly what an operator restart
// produces — the informer replays every AgentSession, but core NATS has no
// replay, so a mid-turn session has no overlay to read and would otherwise sit
// at Active=false until its next turn boundary. The seed derives from the same
// shape as webd's derivePause (parked/terminal phases and any pending
// interaction are NOT working), never from a raw Phase == Running test, which
// would over-report every parked session as active.
func TestAggregator_SeedsActivityFromCRDPhaseUntilAnOverlayArrives(t *testing.T) {
	leakagePending := func(s *spiceboxv1alpha1.AgentSession) {
		s.Status.PendingInteractions = []spiceboxv1alpha1.PendingInteraction{
			{RequestID: "lk1", Category: categories.InfoLeakage, Summary: "share export"},
		}
	}
	toolPending := func(s *spiceboxv1alpha1.AgentSession) {
		s.Status.PendingInteractions = []spiceboxv1alpha1.PendingInteraction{
			{RequestID: "tg1", Category: categories.ToolApproval, Summary: "code_gh"},
		}
	}

	cases := []struct {
		name       string
		phase      string
		mutate     func(*spiceboxv1alpha1.AgentSession)
		wantActive bool
		wantCause  string
	}{
		{name: "Running with nothing pending: seeded active", phase: spiceboxv1alpha1.AgentSessionPhaseRunning, wantActive: true},
		{name: "Pending (runner still coming up): seeded active", phase: spiceboxv1alpha1.AgentSessionPhasePending, wantActive: true},
		{name: "Idle (awaiting the next user message): not active, awaiting_reply", phase: spiceboxv1alpha1.AgentSessionPhaseIdle, wantCause: channelevents.PauseCauseReply},
		{name: "AwaitingRetry: not active, awaiting_retry", phase: spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry, wantCause: channelevents.PauseCauseRetry},
		{name: "Succeeded: not active, complete", phase: spiceboxv1alpha1.AgentSessionPhaseSucceeded, wantCause: channelevents.PauseCauseComplete},
		{name: "Failed: not active, failed", phase: spiceboxv1alpha1.AgentSessionPhaseFailed, wantCause: channelevents.PauseCauseFailed},
		{name: "Running with a pending tool approval: not active, awaiting_approval", phase: spiceboxv1alpha1.AgentSessionPhaseRunning, mutate: toolPending, wantCause: channelevents.PauseCauseApproval},
		{name: "Running with a pending leakage approval: not active, awaiting_leakage_approval", phase: spiceboxv1alpha1.AgentSessionPhaseRunning, mutate: leakagePending, wantCause: channelevents.PauseCauseLeakageApproval},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ag := admind.NewAggregator(testr.New(t))
			s := sessionCR("default", "s1", "support-bot", tc.phase)
			if tc.mutate != nil {
				tc.mutate(s)
			}
			ag.UpsertSession(s)

			st, _, ok := ag.Get("default", "s1")
			require.True(t, ok)
			assert.Equal(t, tc.wantActive, st.Active)
			assert.Equal(t, tc.wantCause, st.ActivityCause)
		})
	}
}

// TestAggregator_PhaseSeedNeverOverridesALiveOverlay pins the direction of the
// correction: once a real turn_activity has been folded, the more-responsive
// overlay owns the working state and a re-upsert of a still-working phase must
// not flip a yielded session back to active.
func TestAggregator_PhaseSeedNeverOverridesALiveOverlay(t *testing.T) {
	ag := admind.NewAggregator(testr.New(t))
	ag.UpsertSession(sessionCR("default", "s1", "support-bot", spiceboxv1alpha1.AgentSessionPhaseRunning))

	// The runner yielded mid-turn (tool wait) while the phase is still Running.
	fold(t, ag, "default", "s1", channelevents.KindTurnActivity,
		channelevents.TurnActivityPayload{Active: false, Cause: channelevents.PauseCauseStopped})
	ag.UpsertSession(sessionCR("default", "s1", "support-bot", spiceboxv1alpha1.AgentSessionPhaseRunning))

	st, _, ok := ag.Get("default", "s1")
	require.True(t, ok)
	assert.False(t, st.Active, "a re-upsert must not re-seed over a live overlay")
	assert.Equal(t, channelevents.PauseCauseStopped, st.ActivityCause)
}

// TestAggregator_DeletedSessionIsNotResurrectedByLateEnvelope proves the
// after-delete resurrection route is closed. handleSessionKill issues a plain
// K8s Delete and the AgentSession finalizer stops the runner pod
// asynchronously, dropping its finalizer without waiting — so DeleteSession
// fires while the runner is still inside its termination grace period and
// still publishing. Lazily re-creating an entry there leaves a blank stub no
// deleter ever visits again.
func TestAggregator_DeletedSessionIsNotResurrectedByLateEnvelope(t *testing.T) {
	ag := admind.NewAggregator(testr.New(t))
	ag.UpsertSession(sessionCR("default", "s1", "support-bot", spiceboxv1alpha1.AgentSessionPhaseRunning))
	ag.DeleteSession("default", "s1")

	fold(t, ag, "default", "s1", channelevents.KindTurnActivity,
		channelevents.TurnActivityPayload{Active: true})

	_, _, ok := ag.Get("default", "s1")
	assert.False(t, ok, "a late envelope must not resurrect a deleted session as a blank stub")
	assert.Empty(t, ag.Snapshot(), "a resurrected stub inflates the sessions list and the ActiveSessions KPI")
}

// TestAggregator_UpsertRevivesATombstonedKey guards the other direction: the
// tombstone must never outlive the CR it describes. A live AgentSession —
// including one whose name is reused — always wins.
func TestAggregator_UpsertRevivesATombstonedKey(t *testing.T) {
	ag := admind.NewAggregator(testr.New(t))
	ag.UpsertSession(sessionCR("default", "s1", "support-bot", spiceboxv1alpha1.AgentSessionPhaseRunning))
	ag.DeleteSession("default", "s1")
	ag.UpsertSession(sessionCR("default", "s1", "support-bot", spiceboxv1alpha1.AgentSessionPhaseRunning))

	fold(t, ag, "default", "s1", channelevents.KindTurnProgress,
		channelevents.TurnProgressPayload{InputTokens: 5000, OutputTokens: 900, ElapsedSeconds: 42})

	st, _, ok := ag.Get("default", "s1")
	require.True(t, ok, "a re-created session must be tracked again")
	assert.Equal(t, int64(5000), st.InputTokens, "and its overlay must fold normally")
}

// TestAggregator_ChunkCadenceKindsAreNotFoldedAtAll pins the perf half: the
// aggregator folds nothing from assistant.stream.delta / tool_session_delta,
// yet before this fix every one of them appended a payload copy to the
// 100-entry ring the console's Recent Activity tab reads and marshalled the
// whole SessionState to every SSE subscriber — per token.
func TestAggregator_ChunkCadenceKindsAreNotFoldedAtAll(t *testing.T) {
	ag := admind.NewAggregator(testr.New(t))
	ag.UpsertSession(sessionCR("default", "s1", "bot", spiceboxv1alpha1.AgentSessionPhaseRunning))
	fold(t, ag, "default", "s1", channelevents.KindNotification,
		channelevents.NotificationPayload{Text: "running terraform plan"})

	ch, cancel := ag.Subscribe()
	defer cancel()

	for i := 0; i <= admind.RingSize; i++ {
		fold(t, ag, "default", "s1", channelevents.KindAssistantStreamDelta,
			map[string]string{"text": "tok"})
	}

	_, recent, ok := ag.Get("default", "s1")
	require.True(t, ok)
	require.Len(t, recent, 1, "token deltas must not evict the ring the Recent Activity tab reads")
	assert.Equal(t, string(channelevents.KindNotification), recent[0].Kind)

	select {
	case fr := <-ch:
		t.Fatalf("chunk-cadence envelope broadcast an SSE frame: %s %s", fr.Event, string(fr.Data))
	default:
	}
}

// forgeOnAttackersSubject publishes, on the ATTACKER session's own out subject
// — the only subtree its per-session NATS JWT permits — an envelope whose
// Session field names the VICTIM. That is the whole attack: the operator's
// subscription is the cluster-wide "ap.session.*.*.out.>", so a principal
// authorized on exactly one session's subject reaches this callback for every
// session if the envelope body is believed.
func forgeOnAttackersSubject(t *testing.T, ag *admind.Aggregator, kind channelevents.Kind, payload any) {
	t.Helper()
	ag.HandleEnvelopeBytes(
		outSubject("default", "attacker", kind),
		envelopeBytes(t, "default", "victim", kind, payload),
	)
}

// TestAggregator_EnvelopeClaimingAnotherSessionCannotDriveIt is the attack, not
// a happy path: every fold the live view performs must key off the NATS subject
// the publisher was authorized on, never off Envelope.Session. The admin console
// is a surface an operator makes decisions from — injected activity, an altered
// working dot, or a rewritten status caption on somebody else's session is a
// forged input to those decisions.
func TestAggregator_EnvelopeClaimingAnotherSessionCannotDriveIt(t *testing.T) {
	cases := []struct {
		name    string
		kind    channelevents.Kind
		payload any
		check   func(t *testing.T, ag *admind.Aggregator, victim admind.SessionState)
	}{
		{
			name:    "turn_progress: victim keeps the CRD's token counters",
			kind:    channelevents.KindTurnProgress,
			payload: channelevents.TurnProgressPayload{InputTokens: 999999, OutputTokens: 888888, ElapsedSeconds: 4242},
			check: func(t *testing.T, _ *admind.Aggregator, victim admind.SessionState) {
				assert.Equal(t, int64(1000), victim.InputTokens)
				assert.Equal(t, int64(200), victim.OutputTokens)
				assert.Zero(t, victim.ElapsedSeconds)
			},
		},
		{
			name:    "turn_activity: victim's working dot is not flipped off",
			kind:    channelevents.KindTurnActivity,
			payload: channelevents.TurnActivityPayload{Active: false, Cause: channelevents.PauseCauseFailed},
			check: func(t *testing.T, _ *admind.Aggregator, victim admind.SessionState) {
				assert.True(t, victim.Active, "a Running session must not be parked by another session's publisher")
				assert.Empty(t, victim.ActivityCause)
			},
		},
		{
			name:    "notification: victim's status caption is not rewritten",
			kind:    channelevents.KindNotification,
			payload: channelevents.NotificationPayload{Text: "deleting the production database"},
			check: func(t *testing.T, _ *admind.Aggregator, victim admind.SessionState) {
				assert.Empty(t, victim.StatusText)
			},
		},
		{
			name:    "plan_update: victim's plan is not replaced",
			kind:    channelevents.KindPlanUpdate,
			payload: map[string]string{"title": "forged plan"},
			check: func(t *testing.T, _ *admind.Aggregator, victim admind.SessionState) {
				assert.Nil(t, victim.Plan)
			},
		},
		{
			name:    "tool_activity: no tool call is attributed to the victim",
			kind:    channelevents.KindToolActivity,
			payload: channelevents.ToolActivityPayload{Tool: "forged_tool"},
			check: func(t *testing.T, ag *admind.Aggregator, _ admind.SessionState) {
				assert.Empty(t, ag.ToolCalls(), "the cluster-wide Live tool-calls view must not show a forged call")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ag := admind.NewAggregator(testr.New(t))
			ag.UpsertSession(sessionCR("default", "victim", "support-bot", spiceboxv1alpha1.AgentSessionPhaseRunning))
			ag.UpsertSession(sessionCR("default", "attacker", "support-bot", spiceboxv1alpha1.AgentSessionPhaseRunning))

			forgeOnAttackersSubject(t, ag, tc.kind, tc.payload)

			victim, recent, ok := ag.Get("default", "victim")
			require.True(t, ok, "the victim session is still tracked")
			tc.check(t, ag, victim)
			assert.Empty(t, recent, "a forged envelope must not land in the victim's Recent Activity ring")

			// The envelope is DROPPED, not silently re-attributed to the
			// session that actually owns the subject. Keying off the subject
			// alone would already spare the victim, but re-attributing would
			// leave a publisher-side session/subject bug invisible — the
			// mismatch has to fail loudly (a logged drop) so it is greppable.
			_, attackerRecent, ok := ag.Get("default", "attacker")
			require.True(t, ok, "the publisher's own session is still tracked")
			assert.Empty(t, attackerRecent,
				"a disagreeing envelope is dropped, not folded into the subject's own session")
		})
	}
}

// TestAggregator_ForgedEnvelopeCannotFabricateASessionRow closes the other half:
// the aggregator creates an entry lazily when NATS runs ahead of the CRD watch,
// so believing Envelope.Session would let one authorized publisher conjure
// arbitrary rows — and their KPI contributions — into the console's sessions
// list for sessions that do not exist.
func TestAggregator_ForgedEnvelopeCannotFabricateASessionRow(t *testing.T) {
	ag := admind.NewAggregator(testr.New(t))
	ag.UpsertSession(sessionCR("default", "attacker", "support-bot", spiceboxv1alpha1.AgentSessionPhaseRunning))

	forgeOnAttackersSubject(t, ag, channelevents.KindTurnActivity,
		channelevents.TurnActivityPayload{Active: true})

	_, _, ok := ag.Get("default", "victim")
	assert.False(t, ok, "a session nobody created must not appear because an envelope claimed it")
	assert.Len(t, ag.Snapshot(), 1, "only the attacker's own real session is listed")
}

// TestAggregator_SubjectThatYieldsNoSessionIdentityIsDropped pins the
// fail-closed half, which is what makes subject a REQUIRED parameter safe: when
// the subject carries no session identity — an empty string from a caller that
// forgot to thread it, an inbound subject, anything off-shape — there is no
// authorized identity to fold against, and the envelope body must not be
// believed as a fallback.
func TestAggregator_SubjectThatYieldsNoSessionIdentityIsDropped(t *testing.T) {
	subjects := map[string]string{
		"empty (a caller that never threaded the subject)": "",
		"an inbound subject, not an out one":               "ap.session.default.s1.in.user_message",
		"not an ap.session subject at all":                 "nonsense",
	}
	for name, subject := range subjects {
		t.Run(name+": nothing is folded", func(t *testing.T) {
			ag := admind.NewAggregator(testr.New(t))
			ag.UpsertSession(sessionCR("default", "s1", "support-bot", spiceboxv1alpha1.AgentSessionPhaseRunning))

			ag.HandleEnvelopeBytes(subject, envelopeBytes(t, "default", "s1", channelevents.KindNotification,
				channelevents.NotificationPayload{Text: "unauthorized caption"}))

			st, recent, ok := ag.Get("default", "s1")
			require.True(t, ok)
			assert.Empty(t, st.StatusText, "an unauthorized subject must not rewrite the status caption")
			assert.Empty(t, recent, "an unauthorized subject must not reach the Recent Activity ring")
		})
	}
}

func TestAggregator_SessionState_CarriesChannelNameAndStartedBy(t *testing.T) {
	ag := admind.NewAggregator(testr.New(t))

	s := sessionCR("default", "s1", "support-bot", "Running")
	s.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{Name: "team-slack", Kind: "slack"}
	s.Annotations = map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: "YWxpY2U"}
	ag.UpsertSession(s)

	st, _, ok := ag.Get("default", "s1")
	require.True(t, ok)
	assert.Equal(t, "slack", st.ChannelKind)
	assert.Equal(t, "team-slack", st.ChannelName, "input channel name surfaced for the sessions table")
	assert.Equal(t, "YWxpY2U", st.StartedBy, "started-by canonical id read from the annotation")

	// A kubectl-driven session (no InputChannel, no annotation) leaves both blank.
	ag.UpsertSession(sessionCR("default", "s2", "support-bot", "Running"))
	st2, _, ok := ag.Get("default", "s2")
	require.True(t, ok)
	assert.Empty(t, st2.ChannelName)
	assert.Empty(t, st2.StartedBy)
}

// TestAggregator_StartedAt_FallsBackToCreationTimestamp proves the sessions
// table always gets a start time: status.startedAt when set, else the CR's
// creation timestamp (no controller stamps status.startedAt today).
func TestAggregator_StartedAt_FallsBackToCreationTimestamp(t *testing.T) {
	ag := admind.NewAggregator(testr.New(t))

	// No status.startedAt, but a creation timestamp → fallback to creation.
	created := time.Date(2026, 6, 20, 8, 0, 0, 0, time.UTC)
	s := &spiceboxv1alpha1.AgentSession{}
	s.Namespace, s.Name = "default", "s-created"
	s.Spec.Class = "bot"
	s.Status.Phase = "Running"
	s.CreationTimestamp = metav1.Time{Time: created}
	ag.UpsertSession(s)

	st, _, ok := ag.Get("default", "s-created")
	require.True(t, ok)
	require.NotNil(t, st.StartedAt, "StartedAt must fall back to CreationTimestamp")
	assert.True(t, st.StartedAt.Equal(created), "fallback uses the CR creation time")

	// status.startedAt present → it wins over creation timestamp.
	started := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	s2 := &spiceboxv1alpha1.AgentSession{}
	s2.Namespace, s2.Name = "default", "s-started"
	s2.CreationTimestamp = metav1.Time{Time: created}
	s2.Status.StartedAt = &metav1.Time{Time: started}
	ag.UpsertSession(s2)

	st2, _, ok := ag.Get("default", "s-started")
	require.True(t, ok)
	require.NotNil(t, st2.StartedAt)
	assert.True(t, st2.StartedAt.Equal(started), "status.startedAt wins when set")
}

func TestAggregator_Approvals_FoldsAllKindsAndDropsOnDelete(t *testing.T) {
	ag := admind.NewAggregator(testr.New(t))

	t0 := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	s := sessionCR("default", "s1", "support-bot", "Running")
	// Since Slice C2 all three approval families park on the single generic
	// PendingInteractions list; the aggregator folds them off it by Category.
	// Tool comes from the publisher's Summary; the generic list carries no
	// requester (Approval.Requester stays empty for every family).
	s.Status.PendingInteractions = []spiceboxv1alpha1.PendingInteraction{
		{RequestID: "tg1", Category: categories.ToolApproval, Summary: "code_gh", RequestedAt: metav1.Time{Time: t0.Add(2 * time.Minute)}},
		{RequestID: "lk1", Category: categories.InfoLeakage, Summary: "share crm export", RequestedAt: metav1.Time{Time: t0.Add(1 * time.Minute)}},
		{RequestID: "ci1", Category: categories.ContentInspection, Summary: "fetch_url flagged", RequestedAt: metav1.Time{Time: t0}},
	}
	ag.UpsertSession(s)

	// SessionState carries all three pending counts.
	st, _, ok := ag.Get("default", "s1")
	require.True(t, ok)
	assert.Equal(t, 1, st.PendingToolGrants)
	assert.Equal(t, 1, st.PendingLeakageApprovals)
	assert.Equal(t, 1, st.PendingContentInspectionApprovals)

	// Approvals() flattens all three kinds, oldest-requested first.
	apps := ag.Approvals()
	require.Len(t, apps, 3)
	assert.Equal(t, "content_inspection", apps[0].Kind, "oldest RequestedAt first")
	// Tool shows the publisher's interaction Summary for every family;
	// Requester is unset (the generic list carries none).
	assert.Equal(t, "fetch_url flagged", apps[0].Tool)
	assert.Empty(t, apps[0].Requester, "the generic list carries no requester")
	assert.Equal(t, "leakage", apps[1].Kind)
	assert.Equal(t, "share crm export", apps[1].Tool)
	assert.Empty(t, apps[1].Requester, "the generic list carries no requester")
	assert.Equal(t, "tool_call", apps[2].Kind)
	assert.Equal(t, "code_gh", apps[2].Tool)
	assert.Empty(t, apps[2].Requester, "the generic list carries no requester")
	// Every approval carries its session + class.
	for _, a := range apps {
		assert.Equal(t, "default", a.Namespace)
		assert.Equal(t, "s1", a.Name)
		assert.Equal(t, "support-bot", a.Class)
	}

	// A fresh upsert with no pending approvals clears them (CRD authoritative).
	ag.UpsertSession(sessionCR("default", "s1", "support-bot", "Running"))
	assert.Empty(t, ag.Approvals(), "re-upsert with empty status clears the queue")

	// Re-add, then delete the session: approvals drop with it.
	ag.UpsertSession(s)
	require.Len(t, ag.Approvals(), 3)
	ag.DeleteSession("default", "s1")
	assert.Empty(t, ag.Approvals(), "DeleteSession drops the session's approvals")
}

func TestAggregator_RecentEventRingCap(t *testing.T) {
	ag := admind.NewAggregator(testr.New(t))
	ag.UpsertSession(sessionCR("default", "s1", "bot", "Running"))

	// Push ringSize+1 envelopes; ring must stay capped at ringSize.
	for i := 0; i <= admind.RingSize; i++ {
		fold(t, ag, "default", "s1", channelevents.KindTurnActivity,
			channelevents.TurnActivityPayload{Active: true})
	}

	_, recent, ok := ag.Get("default", "s1")
	require.True(t, ok)
	require.Len(t, recent, admind.RingSize, "recent-event ring must be capped at RingSize")
}

func TestAggregator_ToolCallRing_CapsNewestFirstAndCarriesClass(t *testing.T) {
	ag := admind.NewAggregator(testr.New(t))
	ag.UpsertSession(sessionCR("default", "s1", "support-bot", "Running"))

	// A tool_activity for a known session carries that session's Class.
	fold(t, ag, "default", "s1", channelevents.KindToolActivity,
		channelevents.ToolActivityPayload{Tool: "code_gh"})
	tc := ag.ToolCalls()
	require.Len(t, tc, 1)
	assert.Equal(t, "code_gh", tc[0].Tool)
	assert.Equal(t, "support-bot", tc[0].Class, "tool call carries the session's AgentClass")
	assert.Equal(t, "default", tc[0].Namespace)
	assert.Equal(t, "s1", tc[0].Name)

	// Push well past the cap; ring stays at ToolCallRingSize, newest-first.
	const total = admind.ToolCallRingSize + 50
	for i := 0; i < total; i++ {
		fold(t, ag, "default", "s1", channelevents.KindToolActivity,
			channelevents.ToolActivityPayload{Tool: fmt.Sprintf("tool-%d", i)})
	}
	tc = ag.ToolCalls()
	require.Len(t, tc, admind.ToolCallRingSize, "global tool-call ring caps at ToolCallRingSize")
	// Newest-first: the last-appended tool is first; the oldest retained is
	// `total - ToolCallRingSize` (the very first per-loop call evicted earliest).
	assert.Equal(t, fmt.Sprintf("tool-%d", total-1), tc[0].Tool, "newest call is first")
	assert.Equal(t, fmt.Sprintf("tool-%d", total-admind.ToolCallRingSize), tc[admind.ToolCallRingSize-1].Tool,
		"oldest retained call is last")

	// Non-tool_activity envelopes do not land in the tool-call ring.
	before := len(ag.ToolCalls())
	fold(t, ag, "default", "s1", channelevents.KindTurnActivity,
		channelevents.TurnActivityPayload{Active: true})
	assert.Len(t, ag.ToolCalls(), before, "only tool_activity feeds the ring")
}

func TestAggregator_SubscribeReceivesUpserts(t *testing.T) {
	ag := admind.NewAggregator(testr.New(t))
	ch, cancel := ag.Subscribe()
	defer cancel()

	ag.UpsertSession(sessionCR("default", "s1", "support-bot", "Running"))

	select {
	case fr := <-ch:
		assert.Equal(t, "session", fr.Event)
		var st admind.SessionState
		require.NoError(t, json.Unmarshal(fr.Data, &st))
		assert.Equal(t, "s1", st.Name)
	case <-time.After(2 * time.Second):
		t.Fatal("no SSE frame within 2s of upsert")
	}

	ag.DeleteSession("default", "s1")
	select {
	case fr := <-ch:
		assert.Equal(t, "remove", fr.Event)
	case <-time.After(2 * time.Second):
		t.Fatal("no remove frame within 2s of delete")
	}
}
