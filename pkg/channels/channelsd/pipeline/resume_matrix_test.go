package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// parkedAtDeterministicName seeds an AgentSession named EXACTLY
// makeSessionName(channel, key) in the given parked phase. The exact name is
// load-bearing: a session under an arbitrary name never hits the
// deterministic-name collision that strands a resume (Create → AlreadyExists →
// re-fetch the parked CR → route with no wake).
func parkedAtDeterministicName(t *testing.T, ch *spiceboxv1alpha1.Channel, key, phase string) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	name := makeSessionName(ch, key)
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ch.Namespace,
			Labels: map[string]string{
				spiceboxv1alpha1.LabelChannelName: ch.Name,
				spiceboxv1alpha1.LabelChannelKey:  sha256HexTest(key),
			},
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByExternalID: "U1",
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "ac1",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: ch.Name, Kind: "fake", Key: key,
				NATSSubjectPrefix: "ap.session.default." + name,
			},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{Phase: phase},
	}
}

// wakeAnnotationOf re-Gets the session and returns its wake-requested-at
// annotation ("" if unset). Used to assert the operator will (or won't)
// respawn the parked session.
func wakeAnnotationOf(t *testing.T, cli client.Client, name string) string {
	t.Helper()
	var got spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, &got), "re-Get session")
	return got.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt]
}

// TestResumeMatrixNoStrand is the anti-strand guard. For each parked phase that
// buckets as NEITHER active nor archived, a new inbound to a session named
// exactly makeSessionName must resume the SAME session
// (never strand): the message is appended (never dropped), the interact gate
// is re-checked on the sender (guard I2), and the wake annotation is written
// exactly for the phases whose pod has exited so the operator respawns them.
func TestResumeMatrixNoStrand(t *testing.T) {
	const key = "thread:C1:1"
	cases := []struct {
		name       string
		phase      string
		wantWake   bool // wake annotation written → operator respawns the parked pod
		wantResume bool // routed to the same (running-or-respawned) session
	}{
		{"AwaitingRetry: plain message resumes + respawns (was stranded)", spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry, true, true},
		{"AwaitingCredentials: appends + reprompts, creds-flow runs it (was stranded)", spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials, false, true},
		{"AwaitingDecision: queues the message mid-decision, never drops (was stranded)", "AwaitingDecision", false, true},
		{"Idle: resumes + respawns (regression guard)", spiceboxv1alpha1.AgentSessionPhaseIdle, true, true},
		{"Running: routes to the live session (regression guard)", spiceboxv1alpha1.AgentSessionPhaseRunning, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := newChannel("c1")
			sess := parkedAtDeterministicName(t, ch, key, tc.phase)
			p, az, mem, _, cli := newPipeline(t, ch, sess)

			dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
				Channel:     ch,
				ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
				ChannelKey:  key,
				MessageText: "please continue",
			})
			require.NoError(t, err, "Deliver")

			// Load-bearing invariant: never routed into a session the operator
			// will not run. A resumed inbound routes to the SAME session (not a
			// freshly created dead-named one) and is not marked NewSession.
			assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome, "outcome")
			assert.Equal(t, sess.Name, dec.Session.Name, "routed to the same parked session")
			assert.False(t, dec.NewSession, "must not spawn a phantom new session")

			// The message is appended (queued, never dropped).
			assert.Len(t, mem.appends, 1, "inbound appended to memory")

			// Guard I2: the interact gate is re-checked on the sender before
			// the message is accepted onto the session.
			assert.Positive(t, az.checkCalls, "interact gate consulted on resume")

			// Wake annotation drives the operator respawn for parked-pod phases.
			got := wakeAnnotationOf(t, cli, sess.Name)
			if tc.wantWake {
				assert.NotEmpty(t, got, "wake annotation written → operator respawns")
			} else {
				assert.Empty(t, got, "no respawn annotation for a live/creds-gated phase")
			}
		})
	}
}

// TestTerminalMatrixNoDeadCRRoute is the anti-strand guard for the ARCHIVED
// slot. A new inbound to a session named exactly makeSessionName that reached a
// terminal phase must NEVER route into that dead CR (the operator won't run
// it). Instead it either forks an inheriting continuation (NewInheriting →
// OutcomeForkPending + an inherit fork-trigger on the terminal parent) or is
// refused loudly (hard Failed → OutcomeRefused, spawn nothing) — with a
// visible notice in every case and no memory append onto the terminal session.
func TestTerminalMatrixNoDeadCRRoute(t *testing.T) {
	const key = "thread:C1:1"
	cases := []struct {
		name          string
		phase         string
		failureReason string
		wantOutcome   channelkinds.Outcome
		wantTrigger   bool // inherit fork-trigger written on the terminal CR
	}{
		{"Succeeded: forks an inheriting continuation (never routes into the dead CR)", spiceboxv1alpha1.AgentSessionPhaseSucceeded, "", channelkinds.OutcomeForkPending, true},
		{"Failed (transient boot): forks an inheriting continuation", spiceboxv1alpha1.AgentSessionPhaseFailed, "MemoryUnavailable", channelkinds.OutcomeForkPending, true},
		{"Failed (hard): refuses loudly, spawns nothing", spiceboxv1alpha1.AgentSessionPhaseFailed, "BudgetExhausted", channelkinds.OutcomeRefused, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := newChannel("c1")
			sess := parkedAtDeterministicName(t, ch, key, tc.phase)
			sess.Status.FailureReason = tc.failureReason
			p, _, mem, _, cli := newPipeline(t, ch, sess)

			dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
				Channel:     ch,
				ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "a@example.com"},
				ChannelKey:  key,
				MessageText: "continue please",
			})
			require.NoError(t, err, "Deliver")

			assert.Equal(t, tc.wantOutcome, dec.Outcome, "outcome")
			assert.NotEqual(t, channelkinds.OutcomeRouted, dec.Outcome, "must never route into the dead CR")
			assert.NotEmpty(t, dec.Notice, "terminal continuation must surface a visible notice")
			assert.Empty(t, mem.appends, "channelsd must not append onto a terminal session")

			pr := pendingRestartOf(t, cli, sess.Name)
			if tc.wantTrigger {
				require.NotNil(t, pr, "inherit fork-trigger must be written on the terminal parent")
				assert.Equal(t, spiceboxv1alpha1.PendingRestartModeInherit, pr.Mode, "Mode=inherit")
				assert.Equal(t, "continue please", pr.NewUserText, "carries the new message")
			} else {
				assert.Nil(t, pr, "a refusal must not write a fork-trigger")
			}
		})
	}
}

// TestResumeInteractDeniedNotAccepted is guard I2 negative: a sender lacking
// interact cannot resume a parked session. The message is denied and never
// appended, so it is not run under the session owner's identity.
func TestResumeInteractDeniedNotAccepted(t *testing.T) {
	const key = "thread:C1:1"
	ch := newChannel("c1")
	sess := parkedAtDeterministicName(t, ch, key, spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry)
	p, az, mem, _, cli := newPipeline(t, ch, sess)
	az.checkResult = false // sender is not authorized to interact

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U2", Email: "b@example.com"},
		ChannelKey:  key,
		MessageText: "let me in",
	})
	require.NoError(t, err, "Deliver")

	assert.Equal(t, channelkinds.OutcomeDeniedByPermission, dec.Outcome, "unauthorized sender denied")
	assert.Empty(t, mem.appends, "denied message must not be appended")
	assert.Empty(t, wakeAnnotationOf(t, cli, sess.Name), "denied message must not respawn the session")
}

// TestDifferentUserTakeoverOfTerminalThread pins the different-user takeover
// behavior in the terminal-continuation block. The terminal parent is owned by
// U1 (parkedAtDeterministicName stamps started-by=U1). A DIFFERENT user (U2)
// gets a NEW session they own, in the same thread: ordinary terminal states
// inherit the transcript, policy/security halts start fresh. The SAME owner (U1)
// keeps today's exact behavior (inherit fork / refuse), unchanged.
func TestDifferentUserTakeoverOfTerminalThread(t *testing.T) {
	const key = "thread:C1:1"
	cases := []struct {
		name          string
		phase         string
		failureReason string
		sender        string // external id of the inbound
		wantOutcome   channelkinds.Outcome
		wantMode      string // "" ⇒ no fork-trigger written
		wantInherit   bool   // meaningful only for takeover mode
		wantNewOwner  string // NewOwnerExternalID; "" for non-takeover
	}{
		{
			name:        "different user, Succeeded: takeover inherits history",
			phase:       spiceboxv1alpha1.AgentSessionPhaseSucceeded,
			sender:      "U2",
			wantOutcome: channelkinds.OutcomeForkPending,
			wantMode:    spiceboxv1alpha1.PendingRestartModeTakeover,
			wantInherit: true, wantNewOwner: "U2",
		},
		{
			name:          "different user, hard non-policy failure: takeover inherits (widened beyond transient)",
			phase:         spiceboxv1alpha1.AgentSessionPhaseFailed,
			failureReason: "SessionExpired",
			sender:        "U2",
			wantOutcome:   channelkinds.OutcomeForkPending,
			wantMode:      spiceboxv1alpha1.PendingRestartModeTakeover,
			wantInherit:   true, wantNewOwner: "U2",
		},
		{
			name:          "different user, policy halt: takeover starts FRESH (no history)",
			phase:         spiceboxv1alpha1.AgentSessionPhaseFailed,
			failureReason: "ToolGuardHalt",
			sender:        "U2",
			wantOutcome:   channelkinds.OutcomeForkPending,
			wantMode:      spiceboxv1alpha1.PendingRestartModeTakeover,
			wantInherit:   false, wantNewOwner: "U2",
		},
		{
			// PhaseHeld is NOT terminal — a forensic hold is frozen and
			// releasable, not finished — so it must not qualify for takeover at
			// all: no fork-trigger, no transcript handed to a stranger. Without
			// the guard in the takeover branch's condition, this session's empty
			// FailureReason (Held never sets it) reads as "not a policy halt" and
			// a different user gets both the takeover AND the full transcript
			// (tool output, memory retrievals) of a session frozen precisely
			// because its agent is suspect.
			name:        "different user, Held: refused, never takes over",
			phase:       spiceboxv1alpha1.AgentSessionPhaseHeld,
			sender:      "U2",
			wantOutcome: channelkinds.OutcomeRefused,
			wantMode:    "",
		},
		{
			name:          "SAME owner, transient failure: existing inherit fork (unchanged)",
			phase:         spiceboxv1alpha1.AgentSessionPhaseFailed,
			failureReason: "MemoryUnavailable",
			sender:        "U1",
			wantOutcome:   channelkinds.OutcomeForkPending,
			wantMode:      spiceboxv1alpha1.PendingRestartModeInherit,
		},
		{
			name:          "SAME owner, hard failure: refused (unchanged)",
			phase:         spiceboxv1alpha1.AgentSessionPhaseFailed,
			failureReason: "BudgetExhausted",
			sender:        "U1",
			wantOutcome:   channelkinds.OutcomeRefused,
			wantMode:      "",
		},
		{
			// The owner was already refused pre-fix (startedBy == sender skips the
			// takeover branch regardless); asserted here so the Held row above is
			// read against BOTH senders, not just the one the guard changes.
			name:        "SAME owner, Held: refused (unchanged)",
			phase:       spiceboxv1alpha1.AgentSessionPhaseHeld,
			sender:      "U1",
			wantOutcome: channelkinds.OutcomeRefused,
			wantMode:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := newChannel("c1")
			sess := parkedAtDeterministicName(t, ch, key, tc.phase)
			sess.Status.FailureReason = tc.failureReason
			p, _, mem, _, cli := newPipeline(t, ch, sess)

			dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
				Channel:     ch,
				ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: identity.RawExternalID(tc.sender), Email: identity.Email(tc.sender + "@example.com")},
				ChannelKey:  key,
				MessageText: "continue please",
			})
			require.NoError(t, err, "Deliver")

			assert.Equal(t, tc.wantOutcome, dec.Outcome, "outcome")
			assert.NotEqual(t, channelkinds.OutcomeRouted, dec.Outcome, "must never route into the dead CR")
			assert.NotEmpty(t, dec.Notice, "every terminal continuation surfaces a visible notice")
			assert.Empty(t, mem.appends, "channelsd must not append onto a terminal session")

			pr := pendingRestartOf(t, cli, sess.Name)
			if tc.wantMode == "" {
				assert.Nil(t, pr, "a refusal must not write a fork-trigger")
				return
			}
			require.NotNil(t, pr, "a continuation must write a fork-trigger on the terminal parent")
			assert.Equal(t, tc.wantMode, pr.Mode, "trigger mode")
			assert.Equal(t, "continue please", pr.NewUserText, "carries the new message")

			if tc.wantMode == spiceboxv1alpha1.PendingRestartModeTakeover {
				assert.Equal(t, tc.wantInherit, pr.InheritHistory, "InheritHistory selects inherit-vs-fresh")
				assert.Equal(t, tc.wantNewOwner, pr.NewOwnerExternalID, "child owner = the taking-over user's external id")
				assert.True(t, strings.HasPrefix(pr.TriggeredBy.String(), "user:"), "TriggeredBy is a user subject")
				assert.NotEqual(t, identity.Subject("user:"), pr.TriggeredBy, "TriggeredBy canonical must be non-empty")
			}
		})
	}
}

// TestTakeoverInheritsWithoutInteractCheck_ByDesign pins the authorization
// shape of the takeover branch, which reads as a gap and is not one. Three
// facts are asserted together because only their conjunction is surprising:
//
//  1. A different user takes the thread over WITHOUT any SpiceDB interact check
//     — checkCalls stays 0 even with the fake wired to deny. The check the
//     sibling same-owner inherit path makes asks "may you continue as this
//     session's owner?", which is the wrong question for a takeover: the child
//     is owned by the NEW user and deliberately does not inherit the parent's
//     interact (restart.go WriteSpiceDBParticipants). The authorization basis
//     is the channel itself — the same basis on which this sender could start
//     their own fresh session, which the archived==nil path below also grants
//     with no SpiceDB call.
//  2. The child inherits the FULL parent transcript for an ordinary terminal
//     reason. This grants exactly one privilege over a fresh start: reading the
//     predecessor's transcript, which holds tool output and memory retrievals
//     never posted to the thread. That cross-user exposure was weighed and
//     accepted; policy halts (IsPolicyHalt) are the carve-out that keeps a
//     guard-stopped conversation out of a new owner's hands.
//  3. The SAME owner is refused for that identical failure reason, so a
//     non-owner is granted strictly more than the owner. That inversion is the
//     designed trigger ("any terminal state") meeting the same-owner refusal
//     rule, not an accident.
//
// RunnerCrash is the reason under test because it is the sharpest case: hard,
// non-policy, and Refuse for the owner. Changing any of the three is a
// product decision; this test is here so it cannot happen by accident.
func TestTakeoverInheritsWithoutInteractCheck_ByDesign(t *testing.T) {
	const key = "thread:C1:crash"

	t.Run("different user, RunnerCrash: inheriting takeover with zero interact checks", func(t *testing.T) {
		ch := newChannel("c1")
		sess := parkedAtDeterministicName(t, ch, key, spiceboxv1alpha1.AgentSessionPhaseFailed)
		sess.Status.FailureReason = "RunnerCrash"
		p, az, _, _, cli := newPipeline(t, ch, sess)
		// Denying is what makes the assertion meaningful: were the branch gated
		// on interact at all, this sender would be refused.
		az.checkResult = false

		dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
			Channel:     ch,
			ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U2", Email: "u2@example.com"},
			ChannelKey:  key,
			MessageText: "taking this over",
		})
		require.NoError(t, err, "Deliver")

		assert.Equal(t, channelkinds.OutcomeForkPending, dec.Outcome, "the takeover proceeds")
		assert.Zero(t, az.checkCalls, "the takeover branch makes no interact check; the channel is the authorization boundary")

		pr := pendingRestartOf(t, cli, sess.Name)
		require.NotNil(t, pr, "takeover fork-trigger written on the terminal parent")
		assert.Equal(t, spiceboxv1alpha1.PendingRestartModeTakeover, pr.Mode)
		assert.True(t, pr.InheritHistory, "RunnerCrash is not a policy halt, so the full transcript carries to the new owner")
		assert.Equal(t, "U2", pr.NewOwnerExternalID, "the child is owned by the taking-over user")
	})

	t.Run("SAME owner, RunnerCrash: refused, so the owner gets strictly less than a stranger", func(t *testing.T) {
		ch := newChannel("c1")
		sess := parkedAtDeterministicName(t, ch, key, spiceboxv1alpha1.AgentSessionPhaseFailed)
		sess.Status.FailureReason = "RunnerCrash"
		p, _, _, _, cli := newPipeline(t, ch, sess)

		dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
			Channel:     ch,
			ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "u1@example.com"},
			ChannelKey:  key,
			MessageText: "let me continue",
		})
		require.NoError(t, err, "Deliver")

		assert.Equal(t, channelkinds.OutcomeRefused, dec.Outcome, "a crashed session re-refuses its own owner")
		assert.Nil(t, pendingRestartOf(t, cli, sess.Name), "a refusal writes no fork-trigger")
	})
}

// TestTakeoverForkTrigger_AttachmentsNotCarried_UserToldAndLogged is the I4
// fix for the takeover-fork site (pipeline.go's writeTakeoverForkTrigger call):
// PendingRestart.NewUserText carries the triggering message's text, but there
// is no field for its attachments. Before the fix, a file attached to the
// message that started a takeover simply vanished — no notice, no log. This
// asserts the returned Notice now says so.
func TestTakeoverForkTrigger_AttachmentsNotCarried_UserToldAndLogged(t *testing.T) {
	const key = "thread:C1:attach-takeover"
	ch := newChannel("c1")
	sess := parkedAtDeterministicName(t, ch, key, spiceboxv1alpha1.AgentSessionPhaseSucceeded)
	p, _, _, _, _ := newPipeline(t, ch, sess)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: identity.RawExternalID("U2"), Email: identity.Email("u2@example.com")},
		ChannelKey:  key,
		MessageText: "continue please",
		Attachments: oneAttachment("F1", "report.pdf", "application/pdf", 2048),
	})
	require.NoError(t, err, "Deliver")
	require.Equal(t, channelkinds.OutcomeForkPending, dec.Outcome)
	require.NotNil(t, dec.Notice, "a different-user takeover must still surface its usual notice")

	args := dec.Notice.Args()
	assert.Contains(t, args.Body, "did not carry over", "the takeover notice must say the attachment did not come along")
	assert.NotEmpty(t, args.NextStep, "an actionable next step (re-send) must accompany the dropped attachment")
}

// TestInheritFork_AttachmentsNotCarried_UserToldAndLogged is the I4 fix for
// the inherit-fork site (pipeline.go's writeInheritForkTrigger call): same
// gap as the takeover case above, but for the SAME-owner continuation path.
func TestInheritFork_AttachmentsNotCarried_UserToldAndLogged(t *testing.T) {
	const key = "thread:C1:attach-inherit"
	ch := newChannel("c1")
	sess := parkedAtDeterministicName(t, ch, key, spiceboxv1alpha1.AgentSessionPhaseSucceeded)
	p, _, _, _, _ := newPipeline(t, ch, sess)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: identity.RawExternalID("U1"), Email: identity.Email("u1@example.com")},
		ChannelKey:  key,
		MessageText: "continue please",
		Attachments: oneAttachment("F1", "report.pdf", "application/pdf", 2048),
	})
	require.NoError(t, err, "Deliver")
	require.Equal(t, channelkinds.OutcomeForkPending, dec.Outcome)
	require.NotNil(t, dec.Notice, "a same-owner inherit fork must still surface its usual notice")

	args := dec.Notice.Args()
	assert.Contains(t, args.Body, "did not carry over", "the inherit notice must say the attachment did not come along")
	assert.NotEmpty(t, args.NextStep, "an actionable next step (re-send) must accompany the dropped attachment")
}

// TestTakeoverForkTrigger_AlreadyPending_AttachmentsStillToldAndLogged covers
// wrote==false: a takeover fork-trigger is already pending (an earlier
// inbound armed it) when a follow-up message carrying an attachment
// arrives. writeTakeoverForkTrigger's first-writer-wins semantics correctly
// leave the existing trigger alone and skip re-announcing the takeover
// itself — repeating that ack per message would be exactly the spam
// TestTakeoverForkTrigger_AttachmentsNotCarried_UserToldAndLogged's
// single-ack comment exists to avoid — but the attachment on THIS message is
// new information the user has not been told about, and must not be
// silently dropped just because wrote is false.
func TestTakeoverForkTrigger_AlreadyPending_AttachmentsStillToldAndLogged(t *testing.T) {
	const key = "thread:C1:attach-takeover-pending"
	ch := newChannel("c1")
	sess := parkedAtDeterministicName(t, ch, key, spiceboxv1alpha1.AgentSessionPhaseSucceeded)
	sess.Status.PendingRestart = &spiceboxv1alpha1.PendingRestart{
		Mode:              spiceboxv1alpha1.PendingRestartModeTakeover,
		NewUserText:       "an earlier follow-up",
		TriggeredBy:       "user:someone-else",
		TargetSessionName: "already-pending-target",
	}
	p, _, _, _, _ := newPipeline(t, ch, sess)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: identity.RawExternalID("U2"), Email: identity.Email("u2@example.com")},
		ChannelKey:  key,
		MessageText: "here's the file you asked for",
		Attachments: oneAttachment("F1", "report.pdf", "application/pdf", 2048),
	})
	require.NoError(t, err, "Deliver")
	require.Equal(t, channelkinds.OutcomeForkPending, dec.Outcome)
	require.NotNil(t, dec.Notice, "an attachment on a message arriving after the fork is already pending must still be told about")

	assert.Equal(t, categories.AttachmentReadFailed, dec.Notice.Category(),
		"a dedicated attachment notice, not a repeat of the takeover ack (which already fired on the earlier inbound)")
	args := dec.Notice.Args()
	assert.Contains(t, args.Body, "doesn't carry file attachments", "must tell the user the file did not come along")
}

// TestInheritFork_AlreadyPending_AttachmentsStillToldAndLogged is the
// inherit-fork mirror of TestTakeoverForkTrigger_AlreadyPending_AttachmentsStillToldAndLogged:
// same wrote==false gap, same fix, for the SAME-owner continuation path.
func TestInheritFork_AlreadyPending_AttachmentsStillToldAndLogged(t *testing.T) {
	const key = "thread:C1:attach-inherit-pending"
	ch := newChannel("c1")
	sess := parkedAtDeterministicName(t, ch, key, spiceboxv1alpha1.AgentSessionPhaseSucceeded)
	sess.Status.PendingRestart = &spiceboxv1alpha1.PendingRestart{
		Mode:              spiceboxv1alpha1.PendingRestartModeInherit,
		NewUserText:       "an earlier follow-up",
		TriggeredBy:       "user:U1",
		TargetSessionName: "already-pending-target",
	}
	p, _, _, _, _ := newPipeline(t, ch, sess)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: identity.RawExternalID("U1"), Email: identity.Email("u1@example.com")},
		ChannelKey:  key,
		MessageText: "here's the file you asked for",
		Attachments: oneAttachment("F1", "report.pdf", "application/pdf", 2048),
	})
	require.NoError(t, err, "Deliver")
	require.Equal(t, channelkinds.OutcomeForkPending, dec.Outcome)
	require.NotNil(t, dec.Notice, "an attachment on a message arriving after the fork is already pending must still be told about")

	assert.Equal(t, categories.AttachmentReadFailed, dec.Notice.Category(),
		"a dedicated attachment notice, not a repeat of the inherit ack (which already fired on the earlier inbound)")
	args := dec.Notice.Args()
	assert.Contains(t, args.Body, "doesn't carry file attachments", "must tell the user the file did not come along")
}
