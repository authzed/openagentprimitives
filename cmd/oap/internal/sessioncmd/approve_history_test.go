package sessioncmd

import (
	"encoding/json"
	"testing"
	"time"

	apiv1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/interactionhistory"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestHistoryConsentDiscovery(t *testing.T) {
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	source := channelevents.SessionRef{Namespace: "ns", Name: "source"}
	sess := &apiv1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: source.Namespace, Name: source.Name, UID: types.UID("uid")}}
	expires := now.Add(time.Minute)
	request := channelevents.InteractionRequestPayload{
		AgentSessionRef: source, Category: categories.GoalExecutionConsent, RequestRef: "consent", Lead: "Allow one private reminder?",
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceRequester, Requester: &channelevents.ExternalIdentity{Kind: "cli", ExternalID: "owner"}},
		Actions:  []channelevents.InteractionAction{{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision}, {ID: "deny", Label: "Deny", Kind: channelevents.ActionKindDecision}}, ExpiresAt: &expires,
	}
	record := interactionhistory.Content{Source: source, SourceUID: string(sess.UID), At: now, Request: &request}
	entry := func(c interactionhistory.Content) memory.Entry {
		b, err := json.Marshal(c)
		require.NoError(t, err)
		return memory.Entry{Kind: interactionhistory.KindName, Content: b, Provenance: &memory.Provenance{Publisher: "system:channelsd"}}
	}
	t.Run("idle source needs no parked interaction and duplicate publication is one request", func(t *testing.T) {
		got, err := collectHistoryPendings(sess, memory.QueryResult{Entries: []memory.Entry{entry(record), entry(record)}}, now)
		require.NoError(t, err)
		require.Len(t, got, 1)
		matched, err := resolvePending(got, "", "ns", "source")
		require.NoError(t, err)
		dispatch, ok := dispatchFor(matched.Kind)
		require.True(t, ok)
		decision := dispatch.buildDecisionPayload(matched.RequestID, "owner@ap.local", "approve")
		require.Equal(t, request.Category, decision.Category)
		require.Equal(t, "consent", decision.RequestRef)
	})
	t.Run("resolution survives reversed order and replay", func(t *testing.T) {
		applied := interactionhistory.Content{Source: source, SourceUID: string(sess.UID), At: now.Add(time.Second), Applied: &channelevents.InteractionAppliedPayload{AgentSessionRef: source, Category: request.Category, RequestRef: request.RequestRef, Outcome: channelevents.OutcomeApproved}}
		got, err := collectHistoryPendings(sess, memory.QueryResult{Entries: []memory.Entry{entry(applied), entry(record), entry(record)}}, now)
		require.NoError(t, err)
		require.Empty(t, got)
	})
	t.Run("expiry is exclusive", func(t *testing.T) {
		got, err := collectHistoryPendings(sess, memory.QueryResult{Entries: []memory.Entry{entry(record)}}, expires)
		require.NoError(t, err)
		require.Empty(t, got)
	})
	t.Run("foreign and recreated sessions cannot be routed here", func(t *testing.T) {
		old := record
		old.SourceUID = "old-uid"
		foreign := record
		foreign.Source.Name = "child"
		got, err := collectHistoryPendings(sess, memory.QueryResult{Entries: []memory.Entry{entry(old), entry(foreign)}}, now)
		require.NoError(t, err)
		require.Empty(t, got)
	})
	t.Run("incomplete history cannot auto select", func(t *testing.T) {
		for _, q := range []memory.QueryResult{{Partial: true}, {Truncated: true}} {
			_, err := collectHistoryPendings(sess, q, now)
			require.ErrorContains(t, err, "incomplete")
		}
	})
	t.Run("untrusted or malformed snapshots fail closed", func(t *testing.T) {
		unsigned := entry(record)
		unsigned.Provenance = nil
		wrong := entry(record)
		wrong.Provenance = &memory.Provenance{Publisher: "runner"}
		malformed := entry(record)
		malformed.Content = json.RawMessage(`{`)
		mismatch := record
		changed := request
		changed.AgentSessionRef.Name = "different"
		mismatch.Request = &changed
		for _, e := range []memory.Entry{unsigned, wrong, malformed, entry(mismatch)} {
			_, err := collectHistoryPendings(sess, memory.QueryResult{Entries: []memory.Entry{e}}, now)
			require.Error(t, err)
		}
	})
	t.Run("changed request fails closed", func(t *testing.T) {
		other := record
		changed := request
		changed.Body = "different reviewed terms"
		other.Request = &changed
		_, err := collectHistoryPendings(sess, memory.QueryResult{Entries: []memory.Entry{entry(record), entry(other)}}, now)
		require.ErrorContains(t, err, "conflicting")
	})
	t.Run("parked and asynchronous approvals remain ambiguous", func(t *testing.T) {
		cp := sess.DeepCopy()
		cp.Status.PendingInteractions = []apiv1.PendingInteraction{{RequestID: "plan", Category: categories.PlanPhase}}
		got, err := collectHistoryPendings(cp, memory.QueryResult{Entries: []memory.Entry{entry(record)}}, now)
		require.NoError(t, err)
		require.Len(t, got, 2)
		_, err = resolvePending(got, "", "ns", "source")
		require.Error(t, err)
	})
}
