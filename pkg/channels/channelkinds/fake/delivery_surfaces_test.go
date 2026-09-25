package fake

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// TestFake_DeliverySurfacesAreOffByDefault is worth more than everything the
// opt-in enables.
//
// A channel kind's Capabilities decide which meta tools an AgentClass is
// offered, so this default is what every bundle in the suite is standing on: a
// flag that silently defaulted on — or leaked from a scenario that forgot to
// restore it — would widen the tool list under 30-odd bundles at once, and the
// ones that assert on tool availability would start passing or failing for
// reasons nothing in their own JSON explains.
func TestFake_DeliverySurfacesAreOffByDefault(t *testing.T) {
	require.False(t, DeliverySurfacesEnabled(),
		"the package must start with the surfaces off; a leak from another test in this process is itself the bug")

	k := Kind{}
	assert.Equal(t, []string{"text", "markdown"}, k.Capabilities(),
		"the default capability set is what every existing bundle's tool list derives from")
	assert.False(t, k.SupportsLiveViewOffer(), "and no live-view surface")
	assert.Nil(t, k.SubChannelSender("live_view_offer", channelkinds.Deps{}),
		"nor a sender for one")
}

// TestFake_EnableDeliverySurfacesAddsBothAndRestores covers the opt-in and,
// just as importantly, that it hands back exactly what it took.
func TestFake_EnableDeliverySurfacesAddsBothAndRestores(t *testing.T) {
	k := Kind{}
	before := k.Capabilities()

	restore := EnableDeliverySurfaces()
	assert.Contains(t, k.Capabilities(), "asset:text/html",
		"respond_to_user gates its `attached` field on an asset:* capability, so without one the field is not in the schema at all")
	assert.True(t, k.SupportsLiveViewOffer())
	assert.NotNil(t, k.SubChannelSender("live_view_offer", channelkinds.Deps{}))

	restore()
	assert.Equal(t, before, k.Capabilities(), "restore must return the exact default set")
	assert.False(t, k.SupportsLiveViewOffer())
	assert.Nil(t, k.SubChannelSender("live_view_offer", channelkinds.Deps{}))
}

// TestFake_LiveViewDeclarationMatchesItsSender pins the honesty invariant in
// BOTH states.
//
// This is the same defect class as the bug the surrounding change fixes: a kind
// that declares a live-view surface it has no sender for makes
// artifact_offer_view publish an envelope, report the offer as sent, and land
// nowhere — a scenario asserting on that tool result would then pass while the
// transport recorded nothing.
func TestFake_LiveViewDeclarationMatchesItsSender(t *testing.T) {
	k := Kind{}
	for _, enabled := range []bool{false, true} {
		name := "surfaces off"
		if enabled {
			name = "surfaces on"
		}
		t.Run(name, func(t *testing.T) {
			if enabled {
				t.Cleanup(EnableDeliverySurfaces())
			}
			hasSender := k.SubChannelSender("live_view_offer", channelkinds.Deps{}) != nil
			assert.Equal(t, hasSender, k.SupportsLiveViewOffer(),
				"the declaration and the sender are one fact; a disagreement is silent in production")
		})
	}
}

// TestFake_LiveViewOfferSenderRecordsTheOffer is what makes the declaration
// true rather than merely consistent: an offer routed here has to be
// observable, or a bundle cannot tell a rendered offer from a dropped one.
func TestFake_LiveViewOfferSenderRecordsTheOffer(t *testing.T) {
	t.Cleanup(EnableDeliverySurfaces())
	t.Cleanup(ResetAllDrivers)

	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "recording-chan"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "fake", AgentClass: "x"},
	}
	sender := Kind{}.SubChannelSender("live_view_offer", channelkinds.Deps{Channel: ch})
	require.NotNil(t, sender)

	payload, err := json.Marshal(channelevents.LiveViewOfferPayload{
		ArtifactID: "artifact-report-1", RendererKind: "html",
	})
	require.NoError(t, err)
	_, err = sender.Send(context.Background(),
		channelkinds.SessionInfo{Namespace: "default", Name: "review-1"},
		channelevents.Envelope{Kind: channelevents.KindLiveViewOffer, Payload: payload})
	require.NoError(t, err)

	offers := DriverFor("default", "recording-chan").LiveViewOffers()
	require.Len(t, offers, 1, "the offer must be observable, or a bundle cannot assert it landed")
	assert.Equal(t, "artifact-report-1", offers[0].Payload.ArtifactID)
	assert.Equal(t, "review-1", offers[0].SessionRef.Name)
}

// TestFake_LiveViewOfferSenderRefusesAnOfferNamingNoArtifact keeps the fixture
// as strict as the transports it stands in for: recording an offer of nothing
// would let a scenario assert a delivery that named no artifact.
func TestFake_LiveViewOfferSenderRefusesAnOfferNamingNoArtifact(t *testing.T) {
	t.Cleanup(EnableDeliverySurfaces())
	t.Cleanup(ResetAllDrivers)

	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "strict-chan"},
		Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "fake", AgentClass: "x"},
	}
	// Register the Driver up front: the sender refuses before it would resolve
	// one, so without this there is nothing to assert stayed empty.
	drv := driverFor(ch)
	sender := Kind{}.SubChannelSender("live_view_offer", channelkinds.Deps{Channel: ch})
	require.NotNil(t, sender)

	payload, err := json.Marshal(channelevents.LiveViewOfferPayload{RendererKind: "html"})
	require.NoError(t, err)
	_, err = sender.Send(context.Background(),
		channelkinds.SessionInfo{Namespace: "default", Name: "review-1"},
		channelevents.Envelope{Kind: channelevents.KindLiveViewOffer, Payload: payload})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "artifactId")
	assert.Empty(t, drv.LiveViewOffers(), "a refused offer must record nothing")
}
