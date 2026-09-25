package main

import (
	"context"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// anchoredSession builds a channel-attached AgentSession anchored to a
// posted opening message (OutputChannel carrying channel_id + thread_ts,
// and the retained opening-text annotation pinnededit.Desired reads),
// mirroring pinnededit/desired_test.go's own `anchored` helper.
func anchoredSession(phase string, pinned *spiceboxv1alpha1.PinnedMessageStatus) *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gh-sess",
			Namespace: "default",
			UID:       types.UID("uid-pinned-1"),
			Labels:    map[string]string{spiceboxv1alpha1.LabelChannelName: "ch"},
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationSessionOpening: "Picked up PR #4",
			},
		},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:        "agent-a",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "ch"},
			OutputChannel: &spiceboxv1alpha1.ChannelBinding{
				External: map[string]string{"channel_id": "C_OUT", "thread_ts": "111.222"},
			},
		},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			Phase:         phase,
			PinnedMessage: pinned,
		},
	}
}

// fakeEditor is a channelkinds.Sender that also implements
// channelkinds.OpeningMessageEditor, recording every EditOpeningMessage
// call so tests can assert on how many edits fired and with what content.
type fakeEditor struct {
	calls []channelkinds.OpeningMessageContent
	err   error
}

func (e *fakeEditor) Send(_ context.Context, _ channelkinds.SessionInfo, _ channelevents.Envelope) (channelkinds.SubChannelSendResult, error) {
	return channelkinds.SubChannelSendResult{}, nil
}

func (e *fakeEditor) EditOpeningMessage(_ context.Context, _ channelkinds.SessionInfo, content channelkinds.OpeningMessageContent) error {
	if e.err != nil {
		return e.err
	}
	e.calls = append(e.calls, content)
	return nil
}

// fixedResolver implements outbound.SenderResolver, always returning s
// (or resolveErr, if set) from SenderFor. The other two methods are unused
// by maybeEditOpeningMessage and stubbed out.
type fixedResolver struct {
	s          channelkinds.Sender
	resolveErr error
}

func (r *fixedResolver) SenderFor(_ context.Context, _ *spiceboxv1alpha1.AgentSession) (channelkinds.Sender, error) {
	if r.resolveErr != nil {
		return nil, r.resolveErr
	}
	return r.s, nil
}

func (r *fixedResolver) SubChannelSenderFor(_ context.Context, _ *spiceboxv1alpha1.AgentSession, _ string) (channelkinds.Sender, error) {
	return nil, nil
}

func (r *fixedResolver) StreamDeltaSinkFor(_ context.Context, _ *spiceboxv1alpha1.AgentSession) (channelkinds.StreamDeltaSink, error) {
	return nil, nil
}

func TestSessionWatcher_EditsPinnedOpening_OnChangeOnly(t *testing.T) {
	ed := &fakeEditor{}
	sess := anchoredSession(spiceboxv1alpha1.AgentSessionPhaseRunning,
		&spiceboxv1alpha1.PinnedMessageStatus{Badge: spiceboxv1alpha1.OpeningBadgeInProgress})
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	w := newSessionWatcher(cli, &fixedResolver{s: ed}, nil)

	w.maybeEditOpeningMessage(context.Background(), testr.New(t), sess)
	w.maybeEditOpeningMessage(context.Background(), testr.New(t), sess)
	require.Len(t, ed.calls, 1, "unchanged status edits once")
	assert.Equal(t, spiceboxv1alpha1.OpeningBadgeInProgress, ed.calls[0].Badge)
	assert.Equal(t, "Picked up PR #4", ed.calls[0].OpeningText)
	assert.Equal(t, "C_OUT", ed.calls[0].Ref.ChannelID)
	assert.Equal(t, "111.222", ed.calls[0].Ref.TS)

	sess.Status.PinnedMessage.Body = "1 finding"
	w.maybeEditOpeningMessage(context.Background(), testr.New(t), sess)
	require.Len(t, ed.calls, 2, "a changed body must trigger a second edit")
	assert.Equal(t, "1 finding", ed.calls[1].Body)
}

func TestSessionWatcher_TerminalFlipToUnfinished(t *testing.T) {
	ed := &fakeEditor{}
	sess := anchoredSession(spiceboxv1alpha1.AgentSessionPhaseFailed, nil)
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	w := newSessionWatcher(cli, &fixedResolver{s: ed}, nil)

	w.maybeEditOpeningMessage(context.Background(), testr.New(t), sess)
	require.Len(t, ed.calls, 1)
	assert.Equal(t, spiceboxv1alpha1.OpeningBadgeUnfinished, ed.calls[0].Badge)

	// The durable dedup annotation must now be stamped on the server object,
	// so a restart (fresh in-memory memo) still does not re-edit.
	require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(sess), sess))
	assert.Equal(t, string(spiceboxv1alpha1.OpeningBadgeUnfinished), sess.Annotations[pinnedFinalAnnotation])

	// A fresh watcher (simulating a channelsd restart: empty pinnedApplied)
	// reading the same, now-annotated session must NOT re-edit.
	w2 := newSessionWatcher(cli, &fixedResolver{s: ed}, nil)
	w2.maybeEditOpeningMessage(context.Background(), testr.New(t), sess)
	assert.Len(t, ed.calls, 1, "durable annotation dedup must survive a fresh watcher")
}

func TestSessionWatcher_EditOpeningMessage_NoCandidateNoop(t *testing.T) {
	ed := &fakeEditor{}
	// No OutputChannel anchor at all -> pinnededit.Desired returns ok=false.
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default", UID: types.UID("uid-2")},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).Build()
	w := newSessionWatcher(cli, &fixedResolver{s: ed}, nil)

	w.maybeEditOpeningMessage(context.Background(), testr.New(t), sess)
	assert.Empty(t, ed.calls, "no anchored opening: nothing to edit")
}

func TestSessionWatcher_EditOpeningMessage_NonEditorSenderNoop(t *testing.T) {
	// A Sender that does NOT implement OpeningMessageEditor (e.g. github,
	// bento) must be a silent no-op, not a panic or error.
	sess := anchoredSession(spiceboxv1alpha1.AgentSessionPhaseRunning,
		&spiceboxv1alpha1.PinnedMessageStatus{Badge: spiceboxv1alpha1.OpeningBadgeInProgress})
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	w := newSessionWatcher(cli, &fixedResolver{s: &capturingSender{}}, nil)

	w.maybeEditOpeningMessage(context.Background(), testr.New(t), sess)
	// No panic and no annotation stamped is the whole assertion; capturingSender
	// records nothing relevant to this path.
}

func TestSessionWatcher_EditOpeningMessage_EditErrorDoesNotMemo(t *testing.T) {
	ed := &fakeEditor{err: assert.AnError}
	sess := anchoredSession(spiceboxv1alpha1.AgentSessionPhaseRunning,
		&spiceboxv1alpha1.PinnedMessageStatus{Badge: spiceboxv1alpha1.OpeningBadgeInProgress})
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	w := newSessionWatcher(cli, &fixedResolver{s: ed}, nil)

	w.maybeEditOpeningMessage(context.Background(), testr.New(t), sess)
	assert.Empty(t, ed.calls, "editor errored; nothing recorded")

	// Retry next tick: once the editor stops erroring, the (unmemoized) edit
	// must still go through.
	ed.err = nil
	w.maybeEditOpeningMessage(context.Background(), testr.New(t), sess)
	require.Len(t, ed.calls, 1, "a failed edit must not be memoized; the retry must fire")
}

// TestSessionWatcher_PinnedOpening_FullTransitionSequence drives one session
// through the full pinned-opening lifecycle -- in_progress, an enrichment
// body, then a concluded outcome badge with a result link -- and asserts the
// three edits landed in order with the expected content. This is the
// end-to-end substitute called for by Task 12 (a bronze/steel bundle would
// need harness work the fake sender/watcher-poll loop don't have yet).
func TestSessionWatcher_PinnedOpening_FullTransitionSequence(t *testing.T) {
	ed := &fakeEditor{}
	sess := anchoredSession(spiceboxv1alpha1.AgentSessionPhaseRunning,
		&spiceboxv1alpha1.PinnedMessageStatus{Badge: spiceboxv1alpha1.OpeningBadgeInProgress})
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	w := newSessionWatcher(cli, &fixedResolver{s: ed}, nil)

	// Edit #1: initial in_progress render, no enrichment or link yet.
	w.maybeEditOpeningMessage(context.Background(), testr.New(t), sess)
	require.Len(t, ed.calls, 1, "initial in_progress status must edit once")
	assert.Equal(t, spiceboxv1alpha1.OpeningBadgeInProgress, ed.calls[0].Badge)
	assert.Empty(t, ed.calls[0].Body, "no enrichment recorded yet")
	assert.Empty(t, ed.calls[0].Link, "no link before conclusion")

	// Edit #2: an enrichment body lands mid-run. in_progress is not a terminal
	// outcome, so pinnededit.Desired leaves the badge alone -- only the body
	// changes.
	sess.Status.PinnedMessage.Body = "1 finding"
	w.maybeEditOpeningMessage(context.Background(), testr.New(t), sess)
	require.Len(t, ed.calls, 2, "a changed body must trigger a second edit")
	assert.Equal(t, spiceboxv1alpha1.OpeningBadgeInProgress, ed.calls[1].Badge, "still running")
	assert.Equal(t, "1 finding", ed.calls[1].Body)
	assert.Empty(t, ed.calls[1].Link)

	// Edit #3: conclusion -- the runner records a terminal outcome badge plus
	// a result link. The enrichment body from edit #2 carries forward
	// unchanged (last-write-wins; this step doesn't touch it).
	sess.Status.PinnedMessage.Badge = spiceboxv1alpha1.OpeningBadgeProblemsFound
	sess.Status.PinnedMessage.Link = "https://example.invalid/pr/4"
	w.maybeEditOpeningMessage(context.Background(), testr.New(t), sess)
	require.Len(t, ed.calls, 3, "conclusion must trigger a third edit")
	assert.Equal(t, spiceboxv1alpha1.OpeningBadgeProblemsFound, ed.calls[2].Badge)
	assert.Equal(t, "1 finding", ed.calls[2].Body, "enrichment body carries into the concluded render")
	assert.Equal(t, "https://example.invalid/pr/4", ed.calls[2].Link)

	// The three edits must have landed strictly in order: in_progress with no
	// body/link, then in_progress with the body, then problems_found with
	// both body and link.
	require.Len(t, ed.calls, 3)
	gotBadges := []spiceboxv1alpha1.OpeningBadge{ed.calls[0].Badge, ed.calls[1].Badge, ed.calls[2].Badge}
	assert.Equal(t, []spiceboxv1alpha1.OpeningBadge{
		spiceboxv1alpha1.OpeningBadgeInProgress,
		spiceboxv1alpha1.OpeningBadgeInProgress,
		spiceboxv1alpha1.OpeningBadgeProblemsFound,
	}, gotBadges, "edits must fire in transition order")
}

// TestSessionWatcher_PinnedOpening_TerminalFailed_InProgressNoOutcome_UnfinishedEdit
// covers the terminal-failure case named separately by Task 12: a session
// that goes Failed having never recorded an outcome. Unlike
// TestSessionWatcher_TerminalFlipToUnfinished (nil PinnedMessage), this
// session already had an in-flight in_progress render when it died, so the
// terminal derivation in pinnededit.Desired must still override it to
// unfinished rather than leaving the stale in_progress badge in place.
func TestSessionWatcher_PinnedOpening_TerminalFailed_InProgressNoOutcome_UnfinishedEdit(t *testing.T) {
	ed := &fakeEditor{}
	sess := anchoredSession(spiceboxv1alpha1.AgentSessionPhaseFailed,
		&spiceboxv1alpha1.PinnedMessageStatus{Badge: spiceboxv1alpha1.OpeningBadgeInProgress})
	cli := fake.NewClientBuilder().WithScheme(makeScheme()).WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	w := newSessionWatcher(cli, &fixedResolver{s: ed}, nil)

	w.maybeEditOpeningMessage(context.Background(), testr.New(t), sess)
	require.Len(t, ed.calls, 1, "terminal Failed with no recorded outcome must edit exactly once")
	assert.Equal(t, spiceboxv1alpha1.OpeningBadgeUnfinished, ed.calls[0].Badge)
	assert.Empty(t, ed.calls[0].Body)
	assert.Empty(t, ed.calls[0].Link)
}
