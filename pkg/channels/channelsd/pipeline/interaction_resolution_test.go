package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/interactionhistory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/parkedprompt"
	"github.com/stretchr/testify/require"
)

type historyOutageMemory struct {
	memory.Memory
	offline bool
}

func (m *historyOutageMemory) Put(ctx context.Context, e memory.Entry) (memory.Entry, error) {
	if m.offline && e.Kind == interactionhistory.KindName {
		return memory.Entry{}, errors.New("history unavailable")
	}
	return m.Memory.Put(ctx, e)
}
func TestInteractionResolutionRecoversHistoryAndDeliveryWithoutRepeatingGrant(t *testing.T) {
	ctx := context.Background()
	registerInteractionCategory(t, channelinteractions.DecideApprovers)
	calls := 0
	channelinteractions.Bind(fixtureInteractionCategory, trackingHandler(&calls, channelinteractions.Outcome{Result: channelevents.OutcomeApproved, OutcomeText: "Approved by Alice", Reason: "Exact reviewed reason"}, nil))
	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	key := sessKeyForFixture()
	var session v1.AgentSession
	require.NoError(t, cli.Get(ctx, key, &session))
	session.UID = "session-uid"
	require.NoError(t, cli.Update(ctx, &session))
	mem := &historyOutageMemory{Memory: newTestMemory(t), offline: true}
	firstNATS := &fakeNATS{}
	first := newTestPipeline(t, cli, &fakeAuthz{checkApproveResult: true}, firstNATS)
	first.Mem = mem
	alice := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}
	notePendingInteractionRequest(t, mem, key, fixtureInteractionCategory, "review", approversFixtureRequest(alice))
	click := mustBuildInteractionDecision(t, key, fixtureInteractionCategory, "review", "approve", alice)
	require.ErrorContains(t, first.HandleInteractionDecision(ctx, click), "history unavailable")
	require.Equal(t, 1, calls)
	require.False(t, sawPublishedKind(firstNATS, channelevents.KindInteractionApplied), "a live approved card must have durable display evidence")
	record, found, err := parkedprompt.Find(ctx, mem, promptScope(key.Namespace, key.Name), "review")
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, record.Resolved)
	require.True(t, record.ResolutionPending)
	// Recreate channelsd, without its decision cache. Recovery uses the retained
	// outcome, including decider and reason, rather than running the handler.
	mem.offline = false
	secondNATS := &fakeNATS{}
	second := newTestPipeline(t, cli, &fakeAuthz{checkApproveResult: true}, secondNATS)
	second.Mem = mem
	require.NoError(t, second.RecoverInteractionResolutions(ctx, &session))
	require.Equal(t, 1, calls)
	recovered := findInteractionAppliedPayload(t, secondNATS, ".out")
	require.Equal(t, "Approved by Alice", recovered.OutcomeText)
	require.Equal(t, "Exact reviewed reason", recovered.Reason)
	require.Equal(t, alice, *recovered.DecidedBy)
	entries, err := mem.Query(ctx, memory.Query{Scope: promptScope(key.Namespace, key.Name), Kinds: []string{interactionhistory.KindName}})
	require.NoError(t, err)
	require.Len(t, entries.Entries, 1)
	var history interactionhistory.Content
	require.NoError(t, json.Unmarshal(entries.Entries[0].Content, &history))
	require.Equal(t, recovered, *history.Applied)
	count := len(secondNATS.subjects)
	require.NoError(t, second.RecoverInteractionResolutions(ctx, &session))
	require.Len(t, secondNATS.subjects, count, "completed delivery must not repeat on every poll")
	require.NoError(t, second.HandleInteractionDecision(ctx, click))
	require.Equal(t, 1, calls)
}

func TestApprovalAndTimeoutKeepOneCanonicalOutcome(t *testing.T) {
	ctx := context.Background()
	registerInteractionCategory(t, channelinteractions.DecideApprovers)
	entered, release := make(chan struct{}), make(chan struct{})
	channelinteractions.Bind(fixtureInteractionCategory, func(context.Context, channelinteractions.Decision) (channelinteractions.Outcome, error) {
		close(entered)
		<-release
		return channelinteractions.Outcome{Result: channelevents.OutcomeApproved}, nil
	})
	cli := newFakeK8sClientWithExistingSession(t, "U_ALICE")
	key := sessKeyForFixture()
	bus := &fakeNATS{}
	p := newTestPipeline(t, cli, &fakeAuthz{checkApproveResult: true}, bus)
	p.Mem = newTestMemory(t)
	alice := channelevents.ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE", Email: "alice@example.com"}
	notePendingInteractionRequest(t, p.Mem, key, fixtureInteractionCategory, "race", approversFixtureRequest(alice))
	approved := make(chan error, 1)
	go func() {
		approved <- p.HandleInteractionDecision(ctx, mustBuildInteractionDecision(t, key, fixtureInteractionCategory, "race", "approve", alice))
	}()
	<-entered
	expired, err := channelevents.BuildEnvelope(key.Namespace, key.Name, channelevents.KindInteractionApplied, channelevents.InteractionAppliedPayload{Category: fixtureInteractionCategory, RequestRef: "race", Outcome: channelevents.OutcomeExpired})
	require.NoError(t, err)
	timeout := make(chan error, 1)
	go func() { timeout <- p.HandleInteractionApplied(ctx, expired) }()
	select {
	case err := <-timeout:
		t.Fatalf("timeout raced past decision: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-approved)
	require.NoError(t, <-timeout)
	record, found, err := parkedprompt.Find(ctx, p.Mem, promptScope(key.Namespace, key.Name), "race")
	require.NoError(t, err)
	require.True(t, found)
	var envelope channelevents.Envelope
	require.NoError(t, json.Unmarshal(record.Resolution, &envelope))
	var outcome channelevents.InteractionAppliedPayload
	require.NoError(t, json.Unmarshal(envelope.Payload, &outcome))
	require.Equal(t, channelevents.OutcomeApproved, outcome.Outcome)
	require.Equal(t, outcome.Outcome, findInteractionAppliedPayload(t, bus, ".out").Outcome)
}
