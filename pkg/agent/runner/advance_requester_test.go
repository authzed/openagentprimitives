// pkg/agent/runner/advance_requester_test.go
//
// Regression tests for the multiplayer tool-call-authz-subject fix: one
// runner pod parks in-process (await_user_message) across an entire
// channel/thread's lifetime, and drainInbox converts each new inbound
// human message into a "user" turn carrying that sender's own
// identity.Subject as memory.Turn.Author. Before this fix, l.authSubject /
// l.authSubjects were resolved ONCE at session start from the session's
// initiator and never advanced — so a lower-privileged participant's tool
// calls silently authorized against the INITIATOR's SpiceDB grants, a
// privilege escalation. advanceRequester (called from drainInbox for every
// drained turn with a non-empty Author) fixes that by moving the
// current-requester component of the resolved subject(s) forward. See the
// Loop.authSubject field comment (loop_deps.go) and advanceRequester's doc
// (loop_subjects.go).
package runner

import (
	"context"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// noopAuthzClient is a minimal toolcheck.Client stub. advanceRequester and
// ResolveAuthSubjects only gate on AuthzCli != nil — neither of these
// tests exercises an actual CheckPermission round-trip — so this fake
// exists solely to make that nil-check pass without a typed-nil interface
// footgun (see AGENTS.md's "never assign a typed-nil pointer directly").
type noopAuthzClient struct{}

func (noopAuthzClient) CheckPermission(_ context.Context, _ *v1.CheckPermissionRequest) (*v1.CheckPermissionResponse, error) {
	return &v1.CheckPermissionResponse{}, nil
}

// currentRequesterClass and startedByClass and bothClass build a minimal
// AgentClass pinning AgentClass.spec.authz.toolCalls.subject to the named
// mode.
func classWithToolCallSubjectMode(mode string) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Authz: &spiceboxv1alpha1.AuthzBlock{
				ToolCalls: &spiceboxv1alpha1.ToolCallsAuthz{Subject: mode},
			},
		},
	}
}

// TestAdvanceRequester_CurrentRequesterMode_TracksLatestSender proves the
// FIX-2 regression: a currentRequester-mode session started by A (the
// initiator stamped on LastInboundCanonicalID at session start) must
// authorize B's tool calls as B once B's inbound turn has drained — NOT as
// the frozen initiator A. Pre-fix, l.authSubject was resolved once at
// session start and never advanced, so B's tool calls would silently run
// with A's SpiceDB grants.
func TestAdvanceRequester_CurrentRequesterMode_TracksLatestSender(t *testing.T) {
	l := newDrainLoop(t)
	l.AuthzCli = noopAuthzClient{}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				slack.LastInboundCanonicalIDAnnotationKey: "alice-canonical",
			},
		},
	}
	l.ResolveAuthSubjects(sess, classWithToolCallSubjectMode("currentRequester"))
	require.Equal(t, "alice-canonical", l.AuthSubject(),
		"session-start resolution must key on the initiator's LastInboundCanonicalID")

	// B (a different, lower-privileged channel member) sends a follow-up
	// while the runner is parked; channelsd writes it as an "inbox" turn
	// stamped with B's Author.
	bobTurn := drainTextTurn(0, "inbox", "do something as me, not the session starter")
	bobTurn.Author = identity.Subject("user:bob-canonical")
	require.NoError(t, l.Memory.Append(ctx, bobTurn))

	_, _, err := l.drainInbox(ctx, 0)
	require.NoError(t, err, "drainInbox must succeed")

	assert.Equal(t, "bob-canonical", l.AuthSubject(),
		"FIX 2: tool-call auth subject must advance to the CURRENT requester (B), not stay frozen on the initiator (A)")
}

// TestAdvanceRequester_StartedByMode_NeverAdvances proves "startedBy" mode
// is unaffected by the FIX-2 advance: the frozen session-initiator subject
// is the intended behavior for that mode, so draining a different sender's
// turn must NOT move l.authSubject.
func TestAdvanceRequester_StartedByMode_NeverAdvances(t *testing.T) {
	l := newDrainLoop(t)
	l.AuthzCli = noopAuthzClient{}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:alice-canonical",
			},
		},
	}
	l.ResolveAuthSubjects(sess, classWithToolCallSubjectMode("startedBy"))
	require.Equal(t, "alice-canonical", l.AuthSubject(), "startedBy mode resolves to the stamped initiator")

	bobTurn := drainTextTurn(0, "inbox", "a different member replies")
	bobTurn.Author = identity.Subject("user:bob-canonical")
	require.NoError(t, l.Memory.Append(ctx, bobTurn))

	_, _, err := l.drainInbox(ctx, 0)
	require.NoError(t, err, "drainInbox must succeed")

	assert.Equal(t, "alice-canonical", l.AuthSubject(),
		"startedBy mode must stay frozen on the initiator even after a different sender's turn drains")
}

// TestAdvanceRequester_EmptyAuthorKeepsPriorSubject proves a drained turn
// with no Author (a non-human/system-authored inbox entry) does not clear
// or otherwise disturb the previously-resolved auth subject.
func TestAdvanceRequester_EmptyAuthorKeepsPriorSubject(t *testing.T) {
	l := newDrainLoop(t)
	l.AuthzCli = noopAuthzClient{}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				slack.LastInboundCanonicalIDAnnotationKey: "alice-canonical",
			},
		},
	}
	l.ResolveAuthSubjects(sess, classWithToolCallSubjectMode("currentRequester"))

	// No Author set (zero value) — an authorless inbox turn.
	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(0, "inbox", "system-originated follow-up")))

	_, _, err := l.drainInbox(ctx, 0)
	require.NoError(t, err, "drainInbox must succeed")

	assert.Equal(t, "alice-canonical", l.AuthSubject(),
		"an authorless drained turn must not clear or change the resolved auth subject")
}

// TestAdvanceRequester_BothMode_AdvancesCurrentRequesterKeepsStartedBy
// proves "both" mode advances only the current-requester component of
// l.authSubjects (rebuilt via setBothSubjects), leaving the frozen
// started-by component untouched.
//
// NB on what the resulting list means: on the primary check path
// pkg/authz/spicedb/toolcheck/check_tool_call.go evaluates an AND across authSubjects — any
// subject lacking the permission sets firstFail and denies — so after the
// hand-off a call requires BOTH the live sender and the initiator to hold it.
// Only the RouteViaSessionGrant branch is first-allow-wins. (An earlier
// revision of this comment claimed an OR across the list; that is true of
// neither path as written.)
func TestAdvanceRequester_BothMode_AdvancesCurrentRequesterKeepsStartedBy(t *testing.T) {
	l := newDrainLoop(t)
	l.AuthzCli = noopAuthzClient{}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				slack.LastInboundCanonicalIDAnnotationKey:       "alice-canonical",
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:alice-canonical",
			},
		},
	}
	l.ResolveAuthSubjects(sess, classWithToolCallSubjectMode("both"))
	require.ElementsMatch(t, []string{"alice-canonical", "alice-canonical"}, l.authSubjects,
		"both-mode with the same initiator/current-requester at session start (pre-existing "+
			"non-deduping behavior, unchanged by this fix)")

	bobTurn := drainTextTurn(0, "inbox", "a different member replies")
	bobTurn.Author = identity.Subject("user:bob-canonical")
	require.NoError(t, l.Memory.Append(ctx, bobTurn))

	_, _, err := l.drainInbox(ctx, 0)
	require.NoError(t, err, "drainInbox must succeed")

	assert.ElementsMatch(t, []string{"bob-canonical", "alice-canonical"}, l.authSubjects,
		"both mode: current-requester element advances to bob while the started-by element (alice) stays frozen")
}

// TestResolveAuthSubjects_BothMode_SetsActingRequester pins the OTHER half of
// "both" mode, which the subjects-list assertions above do not reach: the
// singular ACTING principal.
//
// pkg/platform/pipeline/types.go documents two distinct fields, and they are not
// redundant. Subjects is the authorization SET — check_tool_call.go prefers it
// over Subject whenever it is non-empty, so the SpiceDB tool-call check has
// always been correct in "both" mode. Requester is "the canonical subject of
// the acting user", and dispatchToolUses feeds it from l.authSubject
// (containParams{requester: l.authSubject, …}). "both" widens who may
// AUTHORIZE; it does not mean two people acted, so the acting principal is the
// same as in currentRequester mode — the current requester, falling back to
// started-by.
//
// Before the fix the "both" branch of ResolveAuthSubjects assigned only
// authSubjectStartedBy + authSubjects, so l.authSubject stayed "" for the
// session's whole life and every LLM-path tool call carried an empty
// pipeline.Input.Requester. See TestInfoLeakRead_BothMode_ActingRequester for
// what that costs downstream.
func TestResolveAuthSubjects_BothMode_SetsActingRequester(t *testing.T) {
	l := newDrainLoop(t)
	l.AuthzCli = noopAuthzClient{}
	ctx := memory.WithSystemApproval(context.Background(), "test")

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				slack.LastInboundCanonicalIDAnnotationKey:       "alice-canonical",
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:carol-canonical",
			},
		},
	}
	l.ResolveAuthSubjects(sess, classWithToolCallSubjectMode("both"))
	require.Equal(t, "alice-canonical", l.AuthSubject(),
		"both mode must resolve an ACTING requester (the current requester), not leave it empty")
	require.ElementsMatch(t, []string{"alice-canonical", "carol-canonical"}, l.authSubjects,
		"the authorization SET still carries both principals")

	// The acting principal must advance with the current requester for the same
	// reason the subjects list does: one runner pod serves the whole channel, and
	// attributing bob's read to alice would run the info-leakage view check
	// against the wrong person.
	bobTurn := drainTextTurn(0, "inbox", "a different member replies")
	bobTurn.Author = identity.Subject("user:bob-canonical")
	require.NoError(t, l.Memory.Append(ctx, bobTurn))

	_, _, err := l.drainInbox(ctx, 0)
	require.NoError(t, err, "drainInbox must succeed")

	assert.Equal(t, "bob-canonical", l.AuthSubject(),
		"both mode: the acting requester must advance to the current sender (bob)")
	assert.ElementsMatch(t, []string{"bob-canonical", "carol-canonical"}, l.authSubjects,
		"the started-by element stays frozen while the current-requester element advances")
}

// TestResolveAuthSubjects_BothMode_FallsBackToStartedBy covers the session that
// has no inbound annotation yet (the initiator's very first turn, before
// channelsd stamps LastInboundCanonicalID). The acting principal falls back to
// started-by — the same precedence currentRequester mode uses, and the same one
// setBothSubjects already applies when it drops the empty element.
func TestResolveAuthSubjects_BothMode_FallsBackToStartedBy(t *testing.T) {
	l := newDrainLoop(t)
	l.AuthzCli = noopAuthzClient{}

	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:carol-canonical",
			},
		},
	}
	l.ResolveAuthSubjects(sess, classWithToolCallSubjectMode("both"))

	assert.Equal(t, "carol-canonical", l.AuthSubject(),
		"with no inbound annotation the acting requester falls back to the initiator")
	assert.Equal(t, []string{"carol-canonical"}, l.authSubjects,
		"the empty current-requester element is dropped from the subject list")
}

// TestCurrentUserTurnIndex_DefaultIsUnknown proves a bare Loop — before
// setCurrentUserTurnIndex is ever called — reports -1 ("unknown"), never a
// false turn 0. This is the whole point of storing (index+1) rather than the
// raw index: the atomic's zero value must not collide with a genuine turn 0.
func TestCurrentUserTurnIndex_DefaultIsUnknown(t *testing.T) {
	l := &Loop{}
	assert.Equal(t, -1, l.CurrentUserTurnIndex())
}

// TestCurrentUserTurnIndex_SetThenGet proves setCurrentUserTurnIndex(0) is
// distinguishable from "never set" — both must not read back as -1.
func TestCurrentUserTurnIndex_SetThenGet(t *testing.T) {
	l := &Loop{}

	l.setCurrentUserTurnIndex(0)
	assert.Equal(t, 0, l.CurrentUserTurnIndex(), "turn 0 must not be confused with unknown (-1)")

	l.setCurrentUserTurnIndex(7)
	assert.Equal(t, 7, l.CurrentUserTurnIndex(), "a later set must overwrite the earlier one")
}

// TestHighestUserTurnIndex covers the Run-start seed: the max Index among
// turns a HUMAN authored (role "user" AND a non-empty Author), or -1 for a
// genuinely cold session. Tool-result turns are stored role "user" too but
// carry no Author, and this seed feeds only the preference read, which wants
// the current HUMAN author — so an author-less turn must never win, or a
// resumed session would seed ?turn= to a tool_result and every preference
// read would fall through to the class default.
func TestHighestUserTurnIndex(t *testing.T) {
	// human returns a role "user" turn carrying an Author, as a real inbound
	// message does; a plain drainTextTurn("user", …) models the author-less
	// tool_result turn the resolver must skip.
	human := func(idx int, text string) memory.Turn {
		tt := drainTextTurn(idx, "user", text)
		tt.Author = identity.Subject("user:YWxpY2U")
		return tt
	}
	cases := []struct {
		name  string
		prior []memory.Turn
		want  int
	}{
		{name: "empty transcript: no user turn exists yet", prior: nil, want: -1},
		{
			name: "only non-user roles: still unknown",
			prior: []memory.Turn{
				drainTextTurn(0, "assistant", "hi"),
				drainTextTurn(1, "system_note", "note"),
			},
			want: -1,
		},
		{
			name: "one authored user turn",
			prior: []memory.Turn{
				human(0, "hello"),
				drainTextTurn(1, "assistant", "hi"),
			},
			want: 0,
		},
		{
			name: "several authored user turns out of order: the MAX index wins",
			prior: []memory.Turn{
				human(0, "hello"),
				drainTextTurn(1, "assistant", "hi"),
				human(2, "follow-up"),
				drainTextTurn(3, "assistant", "ok"),
			},
			want: 2,
		},
		{
			name: "later author-less tool_result turn must not win over the earlier human turn",
			prior: []memory.Turn{
				human(0, "hello"),
				drainTextTurn(1, "assistant", "let me check"),
				drainTextTurn(2, "user", "tool result"), // role "user", no Author
				drainTextTurn(3, "assistant", "done"),
				drainTextTurn(4, "user", "another tool result"),
			},
			want: 0,
		},
		{
			name: "only author-less user turns: no human author known",
			prior: []memory.Turn{
				drainTextTurn(0, "user", "tool result"),
				drainTextTurn(1, "assistant", "hi"),
			},
			want: -1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, highestUserTurnIndex(tc.prior))
		})
	}
}

// TestDrainInbox_AdvancesCurrentUserTurnIndex proves drainInbox tracks
// currentUserTurnIndex beside advanceRequester, at the DRAINED turn's own
// runner-assigned index — unconditionally, unlike advanceRequester's
// authz-mode gate, since every turn placed here is a genuine "user" turn
// regardless of whether it carries an Author.
func TestDrainInbox_AdvancesCurrentUserTurnIndex(t *testing.T) {
	l := newDrainLoop(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	assert.Equal(t, -1, l.CurrentUserTurnIndex(), "a fresh Loop starts with no known current user turn")

	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(0, "user", "start")))
	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(1, "assistant", "ok")))
	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(2, "inbox", "follow-up, no author")))

	_, next, err := l.drainInbox(ctx, 2)
	require.NoError(t, err)
	assert.Equal(t, 2, l.CurrentUserTurnIndex(),
		"an authorless drained turn still advances the current-user-turn index — it is a real user turn")

	// A second queued turn, this time authored, must advance it further.
	authored := drainTextTurn(3, "inbox", "a person replies")
	authored.Author = identity.Subject("user:Ym9i")
	require.NoError(t, l.Memory.Append(ctx, authored))

	_, _, err = l.drainInbox(ctx, next)
	require.NoError(t, err)
	assert.Equal(t, 3, l.CurrentUserTurnIndex(), "the index tracks the LATEST drained turn")
}

// TestDrainInbox_MultipleQueuedTurns_LastOneWinsCurrentUserTurnIndex proves
// the "last iteration wins" semantics for a single drain batch that consumes
// several queued inbox turns at once — the same rule advanceRequester itself
// documents for the auth subject.
func TestDrainInbox_MultipleQueuedTurns_LastOneWinsCurrentUserTurnIndex(t *testing.T) {
	l := newDrainLoop(t)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(0, "user", "start")))
	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(1, "assistant", "ok")))
	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(2, "inbox", "first reply")))
	require.NoError(t, l.Memory.Append(ctx, drainTextTurn(3, "inbox", "second reply")))

	placed, _, err := l.drainInbox(ctx, 2)
	require.NoError(t, err)
	require.Len(t, placed, 2)
	assert.Equal(t, 3, l.CurrentUserTurnIndex(), "one batch drain lands on the LAST placed turn's index")
}
