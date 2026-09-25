package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

func TestFormatTranscript_AttributesAndOmitsNote(t *testing.T) {
	msgs := []channelkinds.HistoryMessage{
		{AuthorDisplayName: "Alice Anderson", Text: "use postgres?", TS: "1.1"},
		{AuthorExternalID: "U_BOB", Text: "yes", TS: "1.2"}, // no display name -> id fallback
	}
	got := formatTranscript("Thread history before you were invited", msgs, true)

	assert.Contains(t, got, "[Thread history before you were invited]")
	assert.Contains(t, got, "[earlier messages omitted — use read_thread_history to fetch more]")
	assert.Contains(t, got, "Alice Anderson: use postgres?")
	assert.Contains(t, got, "U_BOB: yes")
}

func TestFormatTranscript_NoNoteWhenNotTruncated(t *testing.T) {
	msgs := []channelkinds.HistoryMessage{{AuthorDisplayName: "Alice", Text: "hi", TS: "1.1"}}
	got := formatTranscript("h", msgs, false)
	assert.NotContains(t, got, "earlier messages omitted")
}

func TestDistinctParticipants_DedupsAndSkipsApps(t *testing.T) {
	msgs := []channelkinds.HistoryMessage{
		{AuthorExternalID: "U_A", AuthorDisplayName: "Alice", AuthorEmail: "a@example.com"},
		{AuthorExternalID: "U_A", AuthorDisplayName: "Alice", AuthorEmail: "a@example.com"},
		{AuthorExternalID: "U_B", AuthorDisplayName: "Bob", AuthorEmail: "b@example.com"},
		{FromApp: true, AuthorExternalID: "B_BOT"},
	}
	got := distinctParticipants(msgs)
	assert.Len(t, got, 2)
	assert.Equal(t, "U_A", got[0].AuthorExternalID)
	assert.Equal(t, "U_B", got[1].AuthorExternalID)
}

// soleSession returns the single AgentSession in the fake client.
func soleSession(t *testing.T, cli client.Client) spiceboxv1alpha1.AgentSession {
	t.Helper()
	var list spiceboxv1alpha1.AgentSessionList
	require.NoError(t, cli.List(context.Background(), &list))
	require.Len(t, list.Items, 1, "expected exactly one AgentSession")
	return list.Items[0]
}

func slackCanonical(t *testing.T, externalID, email string) identity.CanonicalUserID {
	t.Helper()
	p := identity.FromExternal(identity.KindSlack, "", identity.RawExternalID(externalID), identity.Email(email))
	if email == "" {
		// Email-less principal: opt into the synthetic encoding so the
		// canonical form matches the exact prior (pre-fail-closed) value.
		p = p.AllowSynthetic()
	}
	canon, err := p.Canonical()
	require.NoError(t, err)
	return canon
}

func TestDeliver_AdoptsReplyThreadWithNoPriorSession(t *testing.T) {
	ch := newChannel("c1")
	p, az, mem, _, cli := newPipeline(t, ch)
	p.ReadHistory = func(_ context.Context, _ *spiceboxv1alpha1.Channel, _ string, _ channelkinds.ReadHistoryOpts) (channelkinds.HistoryPage, error) {
		return channelkinds.HistoryPage{
			HasMore: true,
			Messages: []channelkinds.HistoryMessage{
				{AuthorExternalID: "U_A", AuthorDisplayName: "Alice", AuthorEmail: "a@example.com", Text: "older", TS: "10.1"},
				{AuthorExternalID: "U_B", AuthorDisplayName: "Bob", AuthorEmail: "b@example.com", Text: "newer", TS: "10.2"},
			},
		}, nil
	}

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U_SUMMONER", Email: "s@example.com"},
		ChannelKey:  "thread:C1:9.9",
		MessageText: "@bot help",
		ThreadEntry: channelkinds.ThreadEntryReply,
		External:    map[string]string{"channel_id": "C1", "thread_ts": "9.9", "message_ts": "11.0"},
	})
	require.NoError(t, err)
	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
	assert.True(t, dec.NewSession)
	assert.True(t, dec.Adopted)
	assert.ElementsMatch(t, []string{"Alice", "Bob"}, dec.GrantedParticipants)

	sess := soleSession(t, cli)
	require.NotNil(t, sess.Spec.InputChannel)
	assert.Equal(t, "mention_only", sess.Spec.InputChannel.RoutingMode)
	assert.Equal(t, "10.1", sess.Annotations[spiceboxv1alpha1.AnnotationBackfilledFromTS])
	assert.Equal(t, "11.0", sess.Annotations[spiceboxv1alpha1.AnnotationBackfilledThroughTS])

	// The transcript is folded into spec.Prompt — history THEN the live
	// mention — so the runner replays it before the question. Nothing is
	// seeded to memory on the new-session path (a memory turn would drain
	// after the cold-start prompt and land out of order).
	assert.Empty(t, mem.appends, "new-session adoption seeds the prompt, not memory")
	inline := sess.Spec.Prompt.Inline
	assert.Contains(t, inline, "Alice: older")
	assert.Contains(t, inline, "earlier messages omitted")
	assert.Contains(t, inline, "@bot help")
	assert.Less(t, strings.Index(inline, "Alice: older"), strings.Index(inline, "@bot help"),
		"transcript must precede the live mention in the prompt")

	// interact_participant written for both thread authors (by canonical id).
	assert.True(t, az.hasParticipantUser(slackCanonical(t, "U_A", "a@example.com")))
	assert.True(t, az.hasParticipantUser(slackCanonical(t, "U_B", "b@example.com")))
}

func TestDeliver_RootMentionDoesNotAdopt(t *testing.T) {
	ch := newChannel("c1")
	p, _, _, _, cli := newPipeline(t, ch)
	called := false
	p.ReadHistory = func(_ context.Context, _ *spiceboxv1alpha1.Channel, _ string, _ channelkinds.ReadHistoryOpts) (channelkinds.HistoryPage, error) {
		called = true
		return channelkinds.HistoryPage{}, nil
	}
	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U_S", Email: "s@example.com"},
		ChannelKey:  "thread:C1:11.0",
		MessageText: "@bot new thread",
		ThreadEntry: channelkinds.ThreadEntryRoot,
		External:    map[string]string{"channel_id": "C1", "thread_ts": "11.0", "message_ts": "11.0"},
	})
	require.NoError(t, err)
	assert.True(t, dec.NewSession)
	assert.False(t, dec.Adopted)
	assert.False(t, called, "root mention must not call ReadHistory")

	sess := soleSession(t, cli)
	require.NotNil(t, sess.Spec.InputChannel)
	assert.Equal(t, "", sess.Spec.InputChannel.RoutingMode)
}

func TestDeliver_AdoptionWithNilReadHistoryStartsPlainSession(t *testing.T) {
	ch := newChannel("c1")
	p, _, _, _, cli := newPipeline(t, ch)
	p.ReadHistory = nil // no backfiller wired
	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U_S", Email: "s@example.com"},
		ChannelKey:  "thread:C1:9.9",
		MessageText: "@bot help",
		ThreadEntry: channelkinds.ThreadEntryReply,
		External:    map[string]string{"channel_id": "C1", "thread_ts": "9.9", "message_ts": "11.0"},
	})
	require.NoError(t, err)
	assert.True(t, dec.NewSession)
	assert.False(t, dec.Adopted, "no backfiller -> not adopted, just a normal new session")

	sess := soleSession(t, cli)
	require.NotNil(t, sess.Spec.InputChannel)
	assert.Equal(t, "", sess.Spec.InputChannel.RoutingMode)
}

// TestDeliver_ArchivedReMentionForksNotReadopts: re-mentioning an archived
// (Succeeded) thread continues via the operator fork (OutcomeForkPending +
// inherit fork-trigger), not by re-adopting/backfilling. channelsd never calls
// ReadHistory on this path, and never creates the session itself.
func TestDeliver_ArchivedReMentionForksNotReadopts(t *testing.T) {
	ch := newChannel("c1")
	arch := archivedSession(t, "old-sess", "thread:C1:9.9", spiceboxv1alpha1.AgentSessionPhaseSucceeded, "U_S")
	arch.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{
		Name: "c1", Kind: "fake", Key: "thread:C1:9.9", RoutingMode: "mention_only",
	}

	p, _, _, _, cli := newPipeline(t, ch, arch)
	called := false
	p.ReadHistory = func(_ context.Context, _ *spiceboxv1alpha1.Channel, _ string, _ channelkinds.ReadHistoryOpts) (channelkinds.HistoryPage, error) {
		called = true
		return channelkinds.HistoryPage{}, nil
	}

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U_S", Email: "s@example.com"},
		ChannelKey:  "thread:C1:9.9",
		MessageText: "@bot back again",
		ThreadEntry: channelkinds.ThreadEntryReply,
		External:    map[string]string{"channel_id": "C1", "thread_ts": "9.9", "message_ts": "12.0"},
	})
	require.NoError(t, err)
	assert.Equal(t, channelkinds.OutcomeForkPending, dec.Outcome, "archived re-mention forks a continuation")
	assert.False(t, dec.NewSession, "channelsd creates no session on the fork path")
	assert.False(t, dec.Adopted, "re-mention of an archived thread forks; it does not re-adopt")
	assert.False(t, called, "must not call ReadHistory when forking a terminal session")

	assert.NotNil(t, pendingRestartOf(t, cli, "old-sess"), "inherit fork-trigger written on the archived parent")
}

func TestDeliver_CatchUpSeedsDeltaForMentionOnlySession(t *testing.T) {
	ch := newChannel("c1")
	existing := existingSession(t, "c1-abc", "U_S", spiceboxv1alpha1.AgentSessionPhaseRunning)
	existing.Spec.InputChannel.RoutingMode = "mention_only"
	// Fixed-width timestamps so lexical comparison matches numeric order,
	// as real Slack ts (10-digit seconds) do.
	existing.Annotations[spiceboxv1alpha1.AnnotationBackfilledThroughTS] = "1008.0"

	p, az, mem, _, cli := newPipeline(t, ch, existing)
	var gotOpts channelkinds.ReadHistoryOpts
	p.ReadHistory = func(_ context.Context, _ *spiceboxv1alpha1.Channel, _ string, opts channelkinds.ReadHistoryOpts) (channelkinds.HistoryPage, error) {
		gotOpts = opts
		return channelkinds.HistoryPage{Messages: []channelkinds.HistoryMessage{
			{AuthorExternalID: "U_ROOT", AuthorDisplayName: "Rita", Text: "thread root", TS: "1005.0"}, // excluded: <= cursor
			{AuthorExternalID: "U_X", AuthorDisplayName: "Xena", Text: "interim chatter", TS: "1009.0"},
			{FromApp: true, FromSelf: true, AuthorDisplayName: "bot", Text: "bot's own reply", TS: "1009.5"}, // excluded: ours
			{FromApp: true, AuthorDisplayName: "alertbot", Text: "[FIRING] disk full", TS: "1009.7"},         // kept: third-party
			{AuthorExternalID: "U_S", AuthorDisplayName: "Sam", Text: "@bot continue", TS: "1012.0"},         // excluded: trigger
		}}, nil
	}

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U_S", Email: "s@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "@bot continue",
		ThreadEntry: channelkinds.ThreadEntryReply,
		External:    map[string]string{"channel_id": "C1", "thread_ts": "1", "message_ts": "1012.0"},
	})
	require.NoError(t, err)
	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)

	assert.Equal(t, "1008.0", gotOpts.AfterTS)
	assert.Equal(t, "1012.0", gotOpts.BeforeTS)

	require.Len(t, mem.appends, 2, "one catch-up transcript turn + the live mention")
	assert.Contains(t, mem.appends[0].turn.Content[0].Text, "Xena: interim chatter")
	assert.NotContains(t, mem.appends[0].turn.Content[0].Text, "thread root", "messages at/before the cursor excluded")
	assert.NotContains(t, mem.appends[0].turn.Content[0].Text, "bot's own reply",
		"the agent's own replies are already in memory and must not be re-seeded")
	assert.Contains(t, mem.appends[0].turn.Content[0].Text, "alertbot: [FIRING] disk full",
		"a third-party app posted content the agent has never seen; only FromSelf is skipped")
	assert.NotContains(t, mem.appends[0].turn.Content[0].Text, "@bot continue", "the trigger message is excluded from the delta")
	assert.Equal(t, "@bot continue", mem.appends[1].turn.Content[0].Text)

	updated := soleSession(t, cli)
	assert.Equal(t, "1012.0", updated.Annotations[spiceboxv1alpha1.AnnotationBackfilledThroughTS])

	assert.False(t, az.hasParticipantUser(slackCanonical(t, "U_X", "")), "catch-up grants no interact")
}

func TestDeliver_NoCatchUpForDefaultRoutingSession(t *testing.T) {
	ch := newChannel("c1")
	existing := existingSession(t, "c1-abc", "U_S", spiceboxv1alpha1.AgentSessionPhaseRunning)
	existing.Spec.InputChannel.RoutingMode = "" // bot-originated thread

	p, _, mem, _, _ := newPipeline(t, ch, existing)
	called := false
	p.ReadHistory = func(_ context.Context, _ *spiceboxv1alpha1.Channel, _ string, _ channelkinds.ReadHistoryOpts) (channelkinds.HistoryPage, error) {
		called = true
		return channelkinds.HistoryPage{}, nil
	}
	_, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U_S", Email: "s@example.com"},
		ChannelKey:  "thread:C1:1",
		MessageText: "ordinary reply",
		ThreadEntry: channelkinds.ThreadEntryReply,
		External:    map[string]string{"channel_id": "C1", "thread_ts": "1", "message_ts": "12.0"},
	})
	require.NoError(t, err)
	assert.False(t, called, "default RoutingMode session: no catch-up, just a plain user turn")
	require.Len(t, mem.appends, 1)
	assert.Equal(t, "ordinary reply", mem.appends[0].turn.Content[0].Text)
}
