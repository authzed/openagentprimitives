package pipeline

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// classWithThreadSeededSlot builds a class declaring one URL-valued slot that
// fills from the thread, with the published value→id chain the reconciler
// would have derived from the tools.
func classWithThreadSeededSlot(autoGrantFrom []string) *spiceboxv1alpha1.AgentClass {
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"},
	}
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{
		Slots: []spiceboxv1alpha1.AuthzSlot{{
			ResourceType:  "http_target",
			Description:   "a URL the agent may reach",
			Permission:    "reachable",
			FillFrom:      []string{"channel_thread"},
			AutoGrantFrom: autoGrantFrom,
		}},
	}
	ac.Status.ResolvedSlots = []spiceboxv1alpha1.ResolvedSlot{{
		ResourceType:    "http_target",
		Permission:      "reachable",
		ValueTransforms: []string{"normalize_url", "sha256"},
	}}
	return ac
}

// threadWithLinks: the summoner (session requester) posts one link, another
// author posts a different one.
func threadWithLinks() channelkinds.HistoryPage {
	return channelkinds.HistoryPage{
		Messages: []channelkinds.HistoryMessage{
			{AuthorExternalID: "U_SUMMONER", AuthorDisplayName: "Sam", AuthorEmail: "s@example.com",
				Text: "the failing build is at <https://ci.example/job/7>", TS: "10.1"},
			{AuthorExternalID: "U_B", AuthorDisplayName: "Bob", AuthorEmail: "b@example.com",
				Text: "also see https://elsewhere.example/x", TS: "10.2"},
		},
	}
}

func seededID(t *testing.T, raw string) authz.ObjectID {
	t.Helper()
	id, err := authz.NewObjectID(raw, []string{"normalize_url", "sha256"})
	require.NoError(t, err)
	return id
}

// TestAdoption_seedsOnlyTrustedAuthorsValues is the security core of the
// channel_thread fill source, exercised through the real Deliver path rather
// than the seeding function alone.
//
// "Found in the thread" must never mean "anyone's value": a thread is
// multi-author, so seeding every message would let any participant make a
// target reachable just by pasting a link into the conversation.
func TestAdoption_seedsOnlyTrustedAuthorsValues(t *testing.T) {
	ch := newChannel("c1")
	// autoGrantFrom unset ⇒ owner only.
	p, az, _, _, _ := newPipeline(t, ch, classWithThreadSeededSlot(nil))
	p.ReadHistory = func(_ context.Context, _ *spiceboxv1alpha1.Channel, _ string, _ channelkinds.ReadHistoryOpts) (channelkinds.HistoryPage, error) {
		return threadWithLinks(), nil
	}

	deliverAdoptionMention(t, p, ch)

	require.Len(t, az.grantedSlots, 1, "exactly the summoner's link must bind")
	assert.Equal(t, "http_target", az.grantedSlots[0].ResourceType)
	assert.Equal(t, seededID(t, "https://ci.example/job/7"), az.grantedSlots[0].ResourceID,
		"the id must be the one the tool's own Check will compute")
	assert.Contains(t, az.grantedSlotsSession, "default/", "grants must be written against the adopted session")
}

// participants widens it to any attributable thread author — defensible only
// for a tightly-controlled channel, and pinned here so the widening is visible.
func TestAdoption_participantsPolicySeedsEveryAuthor(t *testing.T) {
	ch := newChannel("c1")
	p, az, _, _, _ := newPipeline(t, ch, classWithThreadSeededSlot([]string{"participants"}))
	p.ReadHistory = func(_ context.Context, _ *spiceboxv1alpha1.Channel, _ string, _ channelkinds.ReadHistoryOpts) (channelkinds.HistoryPage, error) {
		return threadWithLinks(), nil
	}

	deliverAdoptionMention(t, p, ch)

	ids := []authz.ObjectID{}
	for _, b := range az.grantedSlots {
		ids = append(ids, b.ResourceID)
	}
	assert.ElementsMatch(t, []authz.ObjectID{
		seededID(t, "https://ci.example/job/7"),
		seededID(t, "https://elsewhere.example/x"),
	}, ids)
}

// An explicitly empty autoGrantFrom means the thread is a SOURCE of candidates
// but a human decides each one. Collapsing it into the unset default would
// widen a policy written to be restrictive.
func TestAdoption_noneAutoGrantFromSeedsNothing(t *testing.T) {
	ch := newChannel("c1")
	p, az, _, _, _ := newPipeline(t, ch, classWithThreadSeededSlot([]string{"none"}))
	p.ReadHistory = func(_ context.Context, _ *spiceboxv1alpha1.Channel, _ string, _ channelkinds.ReadHistoryOpts) (channelkinds.HistoryPage, error) {
		return threadWithLinks(), nil
	}

	deliverAdoptionMention(t, p, ch)

	assert.Empty(t, az.grantedSlots, "nothing may auto-grant; every value routes to approval")
}

// A slot that does not opt into channel_thread is not seeded, so fillFrom
// governs this source the way it governs the others.
func TestAdoption_slotNotOptedIntoThreadIsNotSeeded(t *testing.T) {
	ch := newChannel("c1")
	class := classWithThreadSeededSlot(nil)
	class.Spec.Authz.Slots[0].FillFrom = []string{"ask"}
	p, az, _, _, _ := newPipeline(t, ch, class)
	p.ReadHistory = func(_ context.Context, _ *spiceboxv1alpha1.Channel, _ string, _ channelkinds.ReadHistoryOpts) (channelkinds.HistoryPage, error) {
		return threadWithLinks(), nil
	}

	deliverAdoptionMention(t, p, ch)

	assert.Empty(t, az.grantedSlots)
}

// A class with no slots must not pay for, or trip over, any of this.
func TestAdoption_noSlotsSeedsNothing(t *testing.T) {
	ch := newChannel("c1")
	p, az, _, _, _ := newPipeline(t, ch, classWithInteractPolicy(""))
	p.ReadHistory = func(_ context.Context, _ *spiceboxv1alpha1.Channel, _ string, _ channelkinds.ReadHistoryOpts) (channelkinds.HistoryPage, error) {
		return threadWithLinks(), nil
	}

	deliverAdoptionMention(t, p, ch)

	assert.Empty(t, az.grantedSlots)
}

// TestMint_sweepsStaleSlotTuplesBeforeBinding: a channelsd mint must sweep
// whatever a dead predecessor left under this ns/name BEFORE it writes its own
// mint-time binds — otherwise the operator's own sweep (now suppressed for a
// channelsd mint by the pre-stamped finalizer) raced those binds and could
// delete the legitimate authority they just wrote. The sweep (DeleteSlotGrants)
// must run strictly before the first GrantSlots, and the fresh bind must
// survive it.
func TestMint_sweepsStaleSlotTuplesBeforeBinding(t *testing.T) {
	ch := newChannel("c1")
	p, az, _, _, _ := newPipeline(t, ch, classWithThreadSeededSlot(nil))
	p.ReadHistory = func(_ context.Context, _ *spiceboxv1alpha1.Channel, _ string, _ channelkinds.ReadHistoryOpts) (channelkinds.HistoryPage, error) {
		return threadWithLinks(), nil
	}
	// A stale binding a dead predecessor with this name left behind. The mint
	// sweep must wipe it before the fresh thread-seed bind lands.
	az.grantedSlots = []authz.SlotBinding{{ResourceType: "http_target", ResourceID: authz.TrustedObjectID("stale")}}

	deliverAdoptionMention(t, p, ch)

	assert.Equal(t, 1, az.deleteSlotGrantsCalls, "the mint must sweep exactly once before binding")
	require.GreaterOrEqual(t, len(az.slotCallOrder), 2, "both the sweep and the bind must have run")
	assert.Equal(t, "sweep", az.slotCallOrder[0], "the sweep must run BEFORE the first bind")
	assert.Equal(t, "grant", az.slotCallOrder[1], "the bind follows the sweep")

	require.Len(t, az.grantedSlots, 1, "the stale binding was swept; only the fresh thread-seed bind survives")
	assert.Equal(t, seededID(t, "https://ci.example/job/7"), az.grantedSlots[0].ResourceID,
		"the surviving bind is the summoner's link, not the swept stale tuple")
}

// A failed grant write must not fail the inbound: without the grants the agent
// simply has to ask, which is the same place it would have been anyway.
func TestAdoption_seedGrantFailureStillRoutesTheMessage(t *testing.T) {
	ch := newChannel("c1")
	p, az, _, _, _ := newPipeline(t, ch, classWithThreadSeededSlot(nil))
	az.grantSlotsErr = assert.AnError
	p.ReadHistory = func(_ context.Context, _ *spiceboxv1alpha1.Channel, _ string, _ channelkinds.ReadHistoryOpts) (channelkinds.HistoryPage, error) {
		return threadWithLinks(), nil
	}

	dec := deliverAdoptionMention(t, p, ch)
	assert.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
	assert.Empty(t, az.grantedSlots)
}
