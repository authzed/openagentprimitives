package livemirror

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/interactionhistory"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
)

func TestReplayInteractionPreservesRequestAndResolution(t *testing.T) {
	now := time.Now().UTC()
	source := channelevents.SessionRef{Namespace: "demo", Name: "child"}
	request := channelevents.InteractionRequestPayload{AgentSessionRef: source, Category: "plan_phase", RequestRef: "review", Lead: "Approve this plan", Fields: []channelevents.InteractionField{{Label: "Why", Value: "Exact original reason"}}, Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceRequester, Requester: &channelevents.ExternalIdentity{Kind: "email", ExternalID: "human@example.test", Email: "human@example.test"}}, Actions: []channelevents.InteractionAction{{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision}}}
	for _, outcome := range []string{"approved", "denied", "expired"} {
		t.Run(outcome, func(t *testing.T) {
			applied := channelevents.InteractionAppliedPayload{AgentSessionRef: source, Category: request.Category, RequestRef: request.RequestRef, Outcome: outcome, OutcomeText: "Exact outcome text", Reason: "Exact reason"}
			entry := func(c interactionhistory.Content) memory.Entry {
				raw, err := json.Marshal(c)
				require.NoError(t, err)
				return memory.Entry{Kind: interactionhistory.KindName, Content: raw, Provenance: &memory.Provenance{Publisher: "system:channelsd"}}
			}
			first := entry(interactionhistory.Content{Source: source, SourceUID: "child-uid", At: now, Request: &request})
			resolution := entry(interactionhistory.Content{Source: source, SourceUID: "child-uid", At: now.Add(time.Second), Applied: &applied})
			repeated := entry(interactionhistory.Content{Source: source, SourceUID: "child-uid", At: now.Add(2 * time.Second), Request: &request})
			// Unsorted storage results and repeated requests must not clear a decision.
			items, err := replayInteractions([]memory.Entry{resolution, repeated, first})
			require.NoError(t, err)
			require.Len(t, items, 1)
			require.Equal(t, request, *items[0].InteractionRequest)
			require.Equal(t, applied, *items[0].InteractionApplied)
			require.Equal(t, now, items[0].CreatedAt)
			first.Provenance.Publisher = "session:demo/child"
			_, err = replayInteractions([]memory.Entry{first, resolution})
			require.ErrorContains(t, err, "channelsd provenance")
			resolution = entry(interactionhistory.Content{Source: source, SourceUID: "another-instance", At: now, Applied: &applied})
			first.Provenance.Publisher = "system:channelsd"
			items, err = replayInteractions([]memory.Entry{first, resolution})
			require.NoError(t, err)
			require.Nil(t, items[0].InteractionApplied, "a different session instance cannot resolve this card")
		})
	}
}

func TestReadHistoryReplaysApprovalBetweenMessages(t *testing.T) {
	const name = "approval-history"
	at := time.Now().UTC().Truncate(time.Second)
	source := channelevents.SessionRef{Namespace: testScope, Name: name}
	request := channelevents.InteractionRequestPayload{AgentSessionRef: source, Category: "plan_phase", RequestRef: "review", Lead: "Approve original plan", Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceRequester, Requester: &channelevents.ExternalIdentity{Kind: "email", ExternalID: "human@example.test", Email: "human@example.test"}}}
	applied := channelevents.InteractionAppliedPayload{AgentSessionRef: source, Category: request.Category, RequestRef: request.RequestRef, Outcome: "approved", OutcomeText: "Approved"}
	history := []memory.Entry{}
	for _, c := range []interactionhistory.Content{
		{Source: source, SourceUID: "session-uid", At: at.Add(time.Second), Request: &request},
		{Source: source, SourceUID: "session-uid", At: at.Add(2 * time.Second), Applied: &applied},
	} {
		raw, err := json.Marshal(c)
		require.NoError(t, err)
		history = append(history, memory.Entry{Kind: interactionhistory.KindName, Content: raw, Provenance: &memory.Provenance{Publisher: "system:channelsd"}})
	}
	turns := []memory.Entry{historyTestEntry(t, name, 0, "user", "Start", at), historyTestEntry(t, name, 1, "assistant", "Done", at.Add(3*time.Second))}
	queried := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entries := []memory.Entry{}
		switch r.URL.Path {
		case "/memory/turn/" + testScope + "/" + name:
			entries = turns
		case "/memory/interaction_history/" + testScope + "/" + name:
			queried = true
			entries = history
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(memory.QueryResult{Entries: entries}))
	}))
	defer srv.Close()
	result, err := ReadHistory(context.Background(), srv.URL, "webd-token", testScope, name, logr.Discard())
	require.NoError(t, err)
	require.True(t, queried)
	require.Len(t, result.Timeline, 3)
	require.Equal(t, "Start", result.Timeline[0].Text)
	require.Equal(t, "interaction", result.Timeline[1].Kind)
	require.Equal(t, request, *result.Timeline[1].InteractionRequest)
	require.Equal(t, applied, *result.Timeline[1].InteractionApplied)
	require.Equal(t, at.Add(time.Second), result.Timeline[1].CreatedAt)
	require.Equal(t, "Done", result.Timeline[2].Text)
}
