package goals

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/delivery"
	"github.com/authzed/openagentprimitives/pkg/memory"
	meminmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/replydelivery"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
)

// countingReplies counts every acceptance write that reaches storage, on top
// of replyFaults' injected failures: a "post" is one such write. A close
// marker is a refusal, not a post.
type countingReplies struct {
	*replyFaults
	posts int
}

func (m *countingReplies) Put(ctx context.Context, e memory.Entry) (memory.Entry, error) {
	var c replydelivery.Content
	if e.Kind == replydelivery.KindName && !m.rejectWrite && json.Unmarshal(e.Content, &c) == nil && !c.Closed {
		m.posts++
	}
	return m.replyFaults.Put(ctx, e)
}

// groupReplyHarness is a started group run whose dispatcher delivers through
// operator-signed memory with injectable faults.
type groupReplyHarness struct {
	f       *groupRunFixture
	d       *Dispatcher
	faults  *countingReplies
	base    memory.Memory
	sess    *v1.AgentSession
	o       domain.Occurrence
	g       domain.Goal
	payload channelevents.OutboundUserMessagePayload
}

func newGroupReplyHarness(t *testing.T) *groupReplyHarness {
	t.Helper()
	return newGroupReplyHarnessOnKind(t, "fake")
}

// newGroupReplyHarnessOnKind is newGroupReplyHarness with the goal's Channel
// on the named kind.
func newGroupReplyHarnessOnKind(t *testing.T, kind string) *groupReplyHarness {
	t.Helper()
	f := newGroupRunFixtureOnKind(t, kind)
	g, o := f.approve(t)
	priv := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	signer := provenance.NewSigner(priv, "system:operator")
	keys := provenance.MapKeyLookup{{Publisher: signer.Publisher(), KeyID: signer.KeyID()}: priv.Public().(ed25519.PublicKey)}
	base := memory.NewLocal(meminmem.NewBackend(), memory.WithProvenanceVerifier(provenance.NewWriteVerifier(keys)))
	faults := &countingReplies{replyFaults: &replyFaults{Memory: base}}
	d := f.dispatcher()
	d.DeliveryMemory = provenance.NewSigningMemory(faults, signer)
	sess := f.started(t, d, o)
	payload := channelevents.OutboundUserMessagePayload{Text: "This week: three releases."}
	var err error
	payload.Delivery, err = channelevents.NewDeliveryOperation(string(sess.UID), "group-reply", payload)
	require.NoError(t, err)
	return &groupReplyHarness{f: f, d: d, faults: faults, base: base, sess: sess, o: o, g: g, payload: payload}
}

// tick advances past the dispatcher's claim lease and runs one tick.
func (h *groupReplyHarness) tick(t *testing.T) error {
	t.Helper()
	*h.f.clock = h.f.clock.Add(6 * time.Second)
	return h.d.Tick(context.Background())
}

func (h *groupReplyHarness) occurrence(t *testing.T) domain.Occurrence {
	t.Helper()
	o, err := h.f.store.Occurrence(context.Background(), h.o.ID)
	require.NoError(t, err)
	return o
}

func (h *groupReplyHarness) stored(t *testing.T) []memory.Entry {
	t.Helper()
	ctx := memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:operator"), "system:operator")
	res, err := h.base.Query(ctx, memory.Query{Scope: memory.Scope{Kind: "session", ID: "team/" + h.o.SessionName}, Kinds: []string{replydelivery.KindName}})
	require.NoError(t, err)
	return res.Entries
}

func TestGroupReplyDestinationIsTheGroup(t *testing.T) {
	h := newGroupReplyHarness(t)
	prepared, err := h.d.PrepareGoalReply(context.Background(), h.sess, h.payload, nil)
	require.NoError(t, err)
	require.NotNil(t, prepared.Reply)
	assert.Equal(t, "prepared", prepared.Reply.State)
	pin, err := h.g.Execution.Terms.Group.RouteDigest()
	require.NoError(t, err)
	assert.Equal(t, delivery.Destination{Kind: "fake", ChannelUID: "channel-uid", BindingDigest: pin, Recipient: groupRunOwner}, prepared.Reply.Intent.Destination,
		"the reply's audience is the goal's group on its pinned Channel, not any one person")
}

func TestGroupReplyDelivery(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, h *groupReplyHarness)
	}{
		{name: "receiver accepts: prepared -> attempted -> accepted with one post", run: func(t *testing.T, h *groupReplyHarness) {
			require.NoError(t, h.tick(t))
			o := h.occurrence(t)
			require.NotNil(t, o.Reply)
			assert.Equal(t, "accepted", o.Reply.State)
			require.NotNil(t, o.Reply.Receipt)
			assert.NoError(t, o.Reply.Receipt.Validate(o.Reply.Intent))
			assert.Equal(t, 1, h.faults.posts)
			assert.Len(t, h.stored(t), 1)
		}},
		{name: "crash between attempt and accept: retry finds the first post and posts nothing new", run: func(t *testing.T, h *groupReplyHarness) {
			h.faults.loseAck = true
			require.ErrorContains(t, h.tick(t), "reply acknowledgement lost")
			assert.Equal(t, "attempted", h.occurrence(t).Reply.State)
			h.faults.loseAck = false
			require.NoError(t, h.tick(t))
			o := h.occurrence(t)
			assert.Equal(t, "accepted", o.Reply.State)
			require.NotNil(t, o.Reply.Receipt)
			assert.Equal(t, 1, h.faults.posts, "the retry's lookup found the first post")
			assert.Len(t, h.stored(t), 1)
		}},
		{name: "lost post, then the run's window ends: lookup misses and the reply is absent", run: func(t *testing.T, h *groupReplyHarness) {
			h.faults.rejectWrite = true
			require.ErrorContains(t, h.tick(t), "reply not accepted")
			assert.Equal(t, "attempted", h.occurrence(t).Reply.State)
			h.faults.rejectWrite = false
			*h.f.clock = h.sess.CreationTimestamp.Add(time.Duration(h.g.Execution.Terms.Bounds.DurationSeconds) * time.Second)
			require.NoError(t, h.tick(t))
			o := h.occurrence(t)
			assert.Equal(t, "absent", o.Reply.State)
			assert.Nil(t, o.Reply.Receipt)
			assert.Equal(t, 0, h.faults.posts, "nothing was posted after the window closed")
			entries := h.stored(t)
			require.Len(t, entries, 1)
			var c replydelivery.Content
			require.NoError(t, json.Unmarshal(entries[0].Content, &c))
			assert.True(t, c.Closed, "the delivery is durably closed, so a late attempt can never land")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newGroupReplyHarness(t)
			prepared, err := h.d.PrepareGoalReply(context.Background(), h.sess, h.payload, nil)
			require.NoError(t, err)
			require.Equal(t, "prepared", prepared.Reply.State)
			tc.run(t, h)
		})
	}
}

// TestGroupReplyStoreRefusesAnotherDestination: the store re-checks the
// destination against the reviewed group, so a reply addressed anywhere else
// is never prepared.
func TestGroupReplyStoreRefusesAnotherDestination(t *testing.T) {
	h := newGroupReplyHarness(t)
	prepared, err := h.d.PrepareGoalReply(context.Background(), h.sess, h.payload, nil)
	require.NoError(t, err)
	good := prepared.Reply.Intent
	cases := []struct {
		name   string
		mutate func(*delivery.Destination)
	}{
		{name: "another group: denied", mutate: func(d *delivery.Destination) { d.Recipient = "group:other#member" }},
		{name: "another Channel: denied", mutate: func(d *delivery.Destination) { d.ChannelUID = "other-uid" }},
		{name: "another route pin: denied", mutate: func(d *delivery.Destination) { d.BindingDigest = "other" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			intent := good
			tc.mutate(&intent.Destination)
			o := h.occurrence(t)
			o.Reply = nil
			_, err := h.f.store.PrepareReply(context.Background(), o, domain.RunReply{Intent: intent}, *h.f.clock)
			assert.ErrorIs(t, err, domain.ErrDenied)
		})
	}
}
