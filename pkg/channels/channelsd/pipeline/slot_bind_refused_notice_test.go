package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// slotBindRefusedNotices returns every SlotBindRefused interaction-request the
// recorder saw, decoded — the "recorded interactions" the pipeline produced.
func slotBindRefusedNotices(t *testing.T, rec *fakeNATS) []channelevents.InteractionRequestPayload {
	t.Helper()
	var out []channelevents.InteractionRequestPayload
	for i, subj := range rec.subjects {
		if !strings.HasSuffix(subj, ".out."+string(channelevents.KindInteractionRequest)) {
			continue
		}
		var env channelevents.Envelope
		if err := json.Unmarshal(rec.payloads[i], &env); err != nil {
			continue
		}
		var pl channelevents.InteractionRequestPayload
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			continue
		}
		if pl.Category == categories.SlotBindRefused {
			out = append(out, pl)
		}
	}
	return out
}

// TestBindTriggerSlots_PinRefusal_RecordsNotice is the surfacing proof for the
// trigger mint-time path: a GrantSlots refusal wrapping authz.ErrSlotPinned
// must record a session-visible SlotBindRefused notice carrying the refusal
// text, not merely log to the operator.
func TestBindTriggerSlots_PinRefusal_RecordsNotice(t *testing.T) {
	const refusal = "slot already pinned to a different instance: this session is pinned to " +
		"github_pull_request:acme/original; to work on github_pull_request:acme/widgets, " +
		"propose an updated plan naming it — an approved plan moves the pin"

	class := classWithSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "github_pull_request", Permission: "write_memory",
		FillFrom: []string{"trigger"},
	})
	reqs := TriggerSlotRequestsFor(context.Background(), class)
	require.Len(t, reqs, 1)

	az := &fakeAuthz{grantSlotsErr: pinRefusal(refusal, "", "github_pull_request")}
	natsRec := &fakeNATS{}
	p := &Pipeline{Authz: az, NATS: natsRec}
	ch := newGitHubChannel("gh-in", "service:demo")
	ev := channelkinds.InboundEvent{
		Channel: ch, DeliveryEvent: "pull_request",
		RawDelivery: []byte(`{"action":"opened","number":7,"repository":{"full_name":"acme/widgets"},` +
			`"pull_request":{"node_id":"PR_x"}}`),
	}

	p.BindTriggerSlots(context.Background(),
		triggerTestSession(),
		ev, reqs, time.Now())

	notices := slotBindRefusedNotices(t, natsRec)
	require.Len(t, notices, 1, "a pin refusal must be explained to the session, not only logged")
	assert.NotEmpty(t, notices[0].Lead, "the notice needs a headline")
	require.NotNil(t, notices[0].Excerpt, "the refusal text must ride the inert excerpt")
	assert.Contains(t, notices[0].Excerpt.Content, "github_pull_request:acme/original",
		"the notice must carry the pinned instance the refusal named")
	assert.Contains(t, notices[0].Excerpt.Content, "propose an updated plan",
		"the notice must carry the route out the refusal named")
}

// TestSeedThreadSlots_PinRefusal_RecordsNotice is the surfacing proof for the
// thread-seed mint-time path (seedThreadSlots): a trusted author's thread value
// whose GrantSlots is refused by a single-occupancy pin must record the same
// session-visible SlotBindRefused notice carrying the refusal text.
func TestSeedThreadSlots_PinRefusal_RecordsNotice(t *testing.T) {
	const refusal = "slot already pinned to a different instance: this session is pinned to " +
		"web_target:https/old.example; start a new session to target web_target:https/new.example"

	// The requester must canonicalise identically to the message author, so the
	// default owner-only trust policy admits the value. Same FromExternal call
	// seedThreadSlots itself makes for each author.
	requester, err := identity.FromExternal("slack", "", "U1", "alice@example.com").
		AllowSynthetic().Canonical()
	require.NoError(t, err, "canonicalise requester")

	az := &fakeAuthz{grantSlotsErr: pinRefusal(refusal, authz.SlotRebindNever, "web_target")}
	natsRec := &fakeNATS{}
	p := &Pipeline{Authz: az, NATS: natsRec}

	sess := triggerTestSession()
	ev := channelkinds.InboundEvent{
		Channel:     newChannel("c1"),
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U1", Email: "alice@example.com"},
	}
	page := channelkinds.HistoryPage{Messages: []channelkinds.HistoryMessage{{
		AuthorExternalID: "U1",
		AuthorEmail:      "alice@example.com",
		Text:             "please look at https://new.example/thing",
	}}}
	reqs := []authz.ThreadSeedRequest{{
		ResourceType: "web_target",
		Permission:   "read",
		// normalize_url is what marks the slot's values as URLs — the shape the
		// seeder extracts from thread text.
		ValueTransforms: []string{"normalize_url"},
	}}

	p.seedThreadSlots(context.Background(), sess, ev, page, requester, reqs, time.Now().Add(time.Hour))

	notices := slotBindRefusedNotices(t, natsRec)
	require.Len(t, notices, 1, "a pin refusal on the seed path must be explained to the session")
	require.NotNil(t, notices[0].Excerpt, "the refusal text must ride the inert excerpt")
	assert.Contains(t, notices[0].Excerpt.Content, "web_target:https/old.example",
		"the notice must carry the pinned instance the refusal named")
	assert.Contains(t, notices[0].Excerpt.Content, "start a new session",
		"the notice must carry the route out the refusal named")
}

// TestBindTriggerSlots_TransientError_NoNotice guards the scope: a non-pin
// GrantSlots failure is a transient infrastructure problem, not a user-actionable
// commitment, and must stay in the operator log — never announced to the session
// as a pin ruling.
func TestBindTriggerSlots_TransientError_NoNotice(t *testing.T) {
	class := classWithSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "github_pull_request", Permission: "write_memory",
		FillFrom: []string{"trigger"},
	})
	reqs := TriggerSlotRequestsFor(context.Background(), class)
	require.Len(t, reqs, 1)

	az := &fakeAuthz{grantSlotsErr: fmt.Errorf("spicedb unavailable")}
	natsRec := &fakeNATS{}
	p := &Pipeline{Authz: az, NATS: natsRec}
	ch := newGitHubChannel("gh-in", "service:demo")
	ev := channelkinds.InboundEvent{
		Channel: ch, DeliveryEvent: "pull_request",
		RawDelivery: []byte(`{"action":"opened","number":7,"repository":{"full_name":"acme/widgets"},` +
			`"pull_request":{"node_id":"PR_x"}}`),
	}

	p.BindTriggerSlots(context.Background(),
		triggerTestSession(),
		ev, reqs, time.Now())

	assert.Empty(t, slotBindRefusedNotices(t, natsRec),
		"a transient failure is not a pin ruling and must not be announced as one")
}

// TestBindTriggerSlots_PinRefusalWithStoreFault_NoticeQuotesOnlyTheRefusal:
// GrantSlots can join a genuine pin refusal for one type with a transient store
// fault for another. The refusal is still an answer the session must hear — the
// fault must not suppress it — but only the refusal's own text may be quoted:
// the fault's raw store endpoints and gRPC status are operator-only.
func TestBindTriggerSlots_PinRefusalWithStoreFault_NoticeQuotesOnlyTheRefusal(t *testing.T) {
	class := classWithSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "github_pull_request", Permission: "write_memory",
		FillFrom: []string{"trigger"},
	})
	reqs := TriggerSlotRequestsFor(context.Background(), class)
	require.Len(t, reqs, 1)

	joined := errors.Join(
		pinRefusal("slot already pinned to a different instance: pinned to github_pull_request:acme/original", "", "github_pull_request"),
		authz.NewSlotBindFailure(authz.SlotFailureFault, "",
			fmt.Errorf("%w: write slot grants: spicedb unavailable: dial tcp 10.0.0.1:50051", authz.ErrSlotStore), "label"),
	)
	az := &fakeAuthz{grantSlotsErr: fmt.Errorf("spicedb: grant slots: %w", joined)}
	natsRec := &fakeNATS{}
	p := &Pipeline{Authz: az, NATS: natsRec}
	ch := newGitHubChannel("gh-in", "service:demo")
	ev := channelkinds.InboundEvent{
		Channel: ch, DeliveryEvent: "pull_request",
		RawDelivery: []byte(`{"action":"opened","number":7,"repository":{"full_name":"acme/widgets"},` +
			`"pull_request":{"node_id":"PR_x"}}`),
	}

	p.BindTriggerSlots(context.Background(),
		triggerTestSession(),
		ev, reqs, time.Now())

	notices := slotBindRefusedNotices(t, natsRec)
	require.Len(t, notices, 1, "a store fault on another type must not suppress the refusal")
	require.NotNil(t, notices[0].Excerpt)
	assert.Contains(t, notices[0].Excerpt.Content, "github_pull_request:acme/original")
	assert.NotContains(t, notices[0].Excerpt.Content, "dial tcp", "store detail must stay operator-only")
	assert.NotContains(t, notices[0].Excerpt.Content, "spicedb", "store detail must stay operator-only")
}

// The NextStep routes by the REFUSED types' rebind policy, not every binding in
// the call: a rebind:never refusal must route to a new conversation only, never
// to an amendment GrantSlots would refuse every time.
func TestSlotBindRefusedNotice_RebindNeverRefusalRoutesToNewConversation(t *testing.T) {
	class := classWithSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "github_pull_request", Permission: "write_memory",
		FillFrom: []string{"trigger"},
	})
	reqs := TriggerSlotRequestsFor(context.Background(), class)
	require.Len(t, reqs, 1)

	az := &fakeAuthz{grantSlotsErr: pinRefusal("slot already pinned to a different instance: this session is pinned to "+
		"github_pull_request:acme/original for its lifetime", authz.SlotRebindNever, "github_pull_request")}
	natsRec := &fakeNATS{}
	p := &Pipeline{Authz: az, NATS: natsRec}
	ch := newGitHubChannel("gh-in", "service:demo")
	ev := channelkinds.InboundEvent{
		Channel: ch, DeliveryEvent: "pull_request",
		RawDelivery: []byte(`{"action":"opened","number":7,"repository":{"full_name":"acme/widgets"},` +
			`"pull_request":{"node_id":"PR_x"}}`),
	}

	p.BindTriggerSlots(context.Background(), triggerTestSession(), ev, reqs, time.Now())

	notices := slotBindRefusedNotices(t, natsRec)
	require.Len(t, notices, 1)
	assert.Contains(t, notices[0].NextStep, "start a new conversation")
	assert.NotContains(t, notices[0].NextStep, "propose an updated plan")
}

// pinRefusal is the per-type refusal GrantSlots produces, as the channelsd
// pipeline receives it.
func pinRefusal(text, rebind, resourceType string) error {
	return authz.NewSlotBindFailure(authz.SlotFailureRefused, rebind,
		fmt.Errorf("%s: %w", text, authz.ErrSlotPinned), resourceType)
}
