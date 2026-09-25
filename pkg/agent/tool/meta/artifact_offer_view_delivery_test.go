package meta_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/agent/completion"
	"github.com/authzed/openagentprimitives/pkg/agent/completion/kinds/artifactdelivery"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

// This file spans the join the two halves of the change meet at: what
// artifact_offer_view does, and what the artifact-delivered completion
// requirement counts. Neither package's own tests can see it — the tool records
// nothing and the requirement calls no tool — so a change to either side that
// broke the contract would leave both suites green.

const offerDeliverySessUID = types.UID("uid-offer-delivery")

// offerDelivery is one artifact seen from both sides: an ArtifactRender the
// completion requirement lists back from the API server, and the same revision
// recorded in the artifacts service the tools resolve handles through.
type offerDelivery struct {
	sess   *tool.SessionContext
	client client.Client
	svc    *artifacts.Service
	// head is the logical artifact id; render is the CR name the requirement
	// reports and respond_to_user resolves down to.
	head   string
	render string
	// submitted is the result agent_work_complete handed back, or nil when the
	// gate refused.
	submitted *tool.AgentResult
}

func newOfferDelivery(t *testing.T) *offerDelivery {
	t.Helper()

	svc := artifacts.NewService(memory.NewLocal(inmem.NewBackend()), nil)
	scope := memory.Scope{Kind: "session", ID: "default/offer-1"}
	head := svc.NewArtifactID()

	cr := &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ar-offer-1-report", Namespace: "default", UID: types.UID("u-offer-report"),
			Labels: map[string]string{artifacts.LabelArtifactID: head},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
				Kind:       "AgentSession", Name: "offer-1", UID: offerDeliverySessUID,
			}},
		},
		Spec: spiceboxv1alpha1.ArtifactRenderSpec{Kind: "html"},
		Status: spiceboxv1alpha1.ArtifactRenderStatus{
			Phase:      spiceboxv1alpha1.ArtifactRenderPhaseReady,
			OutputMIME: "text/html", OutputFilename: "review.html",
		},
	}
	_, err := svc.FinalizeRevision(memory.WithSystemApproval(context.Background(), "test"), scope, cr)
	require.NoError(t, err, "finalize the report revision")

	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")
	// Seeded with a HUMAN-facing input binding: artifact-delivered holds only a
	// session that was offered respond_to_user, and this fixture's whole point
	// is a session that was and chose the live-view offer instead.
	sessCR := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "offer-1", Namespace: "default", UID: offerDeliverySessUID},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "demo-channel", Kind: "fake"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cr, sessCR).Build()

	d := &offerDelivery{client: c, svc: svc, head: head, render: cr.Name}
	d.sess = &tool.SessionContext{
		Namespace: "default", Name: "offer-1", AgentSessionUID: offerDeliverySessUID,
		K8sClient:    c,
		State:        state.NewRegistry(state.Deps{}),
		SubmitResult: func(r tool.AgentResult) { d.submitted = &r },
	}
	return d
}

// offerTheView calls the real artifact_offer_view over this fixture and returns
// its result plus how many envelopes reached the wire.
func (d *offerDelivery) offerTheView(t *testing.T) (tool.Result, int) {
	t.Helper()
	return d.offerTheViewOn(t, "slack")
}

// offerTheViewOn is offerTheView against a named transport, so a case can pick
// one that has a live-view surface and one that does not.
func (d *offerDelivery) offerTheViewOn(t *testing.T, channelKind string) (tool.Result, int) {
	t.Helper()
	published := 0
	tl := meta.NewArtifactOfferView(meta.ArtifactOfferViewConfig{
		Artifacts: d.svc, AvailableKinds: []string{"html"},
		ChannelKind:       channelKind,
		NATSPublish:       func(context.Context, string, []byte) error { published++; return nil },
		NATSSubjectPrefix: "ap.session.default.offer-1",
	})
	args, err := json.Marshal(map[string]any{"artifact_id": d.head})
	require.NoError(t, err)
	res, execErr := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, d.sess)
	require.NoError(t, execErr)
	return res, published
}

// reply calls the real respond_to_user. The capability set is slack's own
// (pkg/channels/channelkinds/slack/kind.go), because respond_to_user only
// offers `attached` on a channel advertising asset:*, and a fixture that
// pretended otherwise would test a delivery no transport performs.
//
// attach=false is the captured session's actual call: summary text, no
// `attached` field, an artifact left behind.
func (d *offerDelivery) reply(t *testing.T, attach bool) tool.Result {
	t.Helper()
	tl := meta.New(meta.RespondConfig{
		Capabilities:      []string{"text", "markdown", "asset:text/html"},
		ChannelKind:       "slack",
		Client:            d.client,
		Artifacts:         d.svc,
		NATSPublish:       func(context.Context, string, []byte) error { return nil },
		NATSSubjectPrefix: "ap.session.default.offer-1",
	})
	call := map[string]any{"text": "Done — full report attached below."}
	if attach {
		call["attached"] = []string{d.render}
	}
	args, err := json.Marshal(call)
	require.NoError(t, err)
	res, execErr := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, d.sess)
	require.NoError(t, execErr)
	return res
}

// finish calls the real agent_work_complete with artifact-delivered declared,
// exactly as an AgentClass that opted into it produces.
func (d *offerDelivery) finish(t *testing.T) tool.Result {
	t.Helper()
	tl := meta.NewAgentWorkComplete(meta.CompletionConfig{
		Requirements: []string{artifactdelivery.Key},
		RecordBypass: func(context.Context, completion.Bypass) error { return nil },
	})
	res, execErr := tl.Execute(context.Background(),
		json.RawMessage(`{"summary":"review delivered"}`), d.sess)
	require.NoError(t, execErr, "a refusal is a tool result, not a Go error")
	return res
}

// unmet drives the requirement through the registry, the same lookup
// agent_work_complete uses.
func (d *offerDelivery) unmet(t *testing.T) []completion.Unmet {
	t.Helper()
	out, err := completion.Evaluate(context.Background(),
		[]string{artifactdelivery.Key}, completion.Input{Session: d.sess})
	require.NoError(t, err, "the requirement must be evaluable on this session")
	return out
}

// TestArtifactDelivered_OfferingALiveViewIsNotDelivering is the ruling on
// whether a live-view offer can satisfy artifact-delivered. It cannot, and this
// is the test that keeps it that way.
//
// Two independent reasons, either sufficient on its own:
//
//  1. The runner cannot know an offer RENDERED. artifact_offer_view publishes an
//     envelope and returns; the sender runs in another process (channelsd, or
//     the user's own oap for a client-hosted kind) and no result travels back.
//     Anything counted here would therefore be the CALL, not the render — the
//     exact shape this requirement exists to refuse. It already declines to
//     count "the render went Ready" for the same reason.
//
//  2. An offer carries no bytes. The button mints a fresh signed link at CLICK
//     time against webd, refuses a viewer without interact permission, and
//     points at a view of a session that is about to end. Treating it as
//     delivery would let a round finish with nothing durable in anyone's hands.
func TestArtifactDelivered_OfferingALiveViewIsNotDelivering(t *testing.T) {
	d := newOfferDelivery(t)

	res, published := d.offerTheView(t)
	require.False(t, res.IsError, "the offer itself must succeed: %s", res.Content)
	require.Equal(t, 1, published, "the offer envelope did go out — this is the strongest case for counting it")

	unmet := d.unmet(t)
	require.Len(t, unmet, 1,
		"a session whose only handover was an offer has put nothing in anyone's hands and must not call its work done")
	assert.Equal(t, artifactdelivery.Key, unmet[0].Key)
	assert.Contains(t, unmet[0].Missing, d.render, "and it must still name the artifact to deliver")
	assert.Contains(t, unmet[0].Missing, "artifact_offer_view",
		"the model just called that and was refused anyway; the message has to say so, or it will call it again")
}

// TestCapturedSession_ReplyWithoutAttachingThenOffer_IsRefused replays the tool
// sequence a real reviewbot round actually ran:
//
//	artifact_prepare → respond_to_user (no `attached`) → artifact_offer_view →
//	agent_work_complete
//
// That round reached its operator only because Slack could render the offer and
// they clicked it. Nothing in the session established that. The requirement has
// to refuse it, and the refusal has to name both the artifact and why the offer
// the agent just made did not count.
func TestCapturedSession_ReplyWithoutAttachingThenOffer_IsRefused(t *testing.T) {
	d := newOfferDelivery(t)

	reply := d.reply(t, false)
	require.False(t, reply.IsError, "the reply itself succeeds — that is what made this invisible: %s", reply.Content)

	offered, published := d.offerTheView(t)
	require.False(t, offered.IsError, "content: %s", offered.Content)
	require.Equal(t, 1, published)

	res := d.finish(t)
	assert.True(t, res.IsError, "a round that handed over nothing must not be able to call itself done")
	assert.False(t, res.Terminal, "and the refusal must not end the session")
	assert.Nil(t, d.submitted, "nothing may be submitted for a refused completion")
	assert.Contains(t, res.Content, d.render, "the refusal names the artifact still in nobody's hands")
	assert.Contains(t, res.Content, "artifact_offer_view",
		"and says why the offer it just made was not the delivery, or its next move is to make it again")
}

// TestCapturedSession_AttachingAndOfferingCompletes is the corrected sequence
// the reviewbot bundle now instructs, and the property the owner asked to be
// pinned: the report is attached to the thread AND offered as a live view, and
// the round finishes clean.
//
// Both halves are asserted to have happened — the delivery record for the
// attachment, the published envelope for the offer — so this cannot pass on one
// of them alone.
func TestCapturedSession_AttachingAndOfferingCompletes(t *testing.T) {
	d := newOfferDelivery(t)

	reply := d.reply(t, true)
	require.False(t, reply.IsError, "content: %s", reply.Content)

	offered, published := d.offerTheView(t)
	require.False(t, offered.IsError, "content: %s", offered.Content)
	assert.Equal(t, 1, published, "the live view was offered as well, not instead")

	assert.Empty(t, d.unmet(t), "the attachment is what reached the user")

	res := d.finish(t)
	assert.False(t, res.IsError, "content: %s", res.Content)
	assert.True(t, res.Terminal, "having delivered both ways, the round finishes")
	require.NotNil(t, d.submitted, "and submits its result")
}

// TestCapturedSession_OnAChannelWithNoLiveViewSurface_NothingClaimsDelivery is
// the same sequence on the transport that makes it dangerous.
//
// Where the captured session ran, the offer rendered and the operator clicked
// it. On a channel with no live-view surface the identical sequence delivers
// NOTHING — and before this change the transcript said "live-view offer sent —
// the user can click to open it" all the same. Two things now have to hold at
// once: the tool must not claim a render, and the gate must refuse the round.
func TestCapturedSession_OnAChannelWithNoLiveViewSurface_NothingClaimsDelivery(t *testing.T) {
	d := newOfferDelivery(t)

	reply := d.reply(t, false)
	require.False(t, reply.IsError, "content: %s", reply.Content)

	offered, published := d.offerTheViewOn(t, "fake")
	require.False(t, offered.IsError, "a channel without the surface is not the agent's failure: %s", offered.Content)
	assert.Zero(t, published, "there is no sender for it here; publishing would only manufacture the false success")
	assert.NotContains(t, offered.Content, "can click",
		"nothing was rendered, so the transcript must not tell the model — and through it the user — that a button exists")

	res := d.finish(t)
	assert.True(t, res.IsError,
		"neither path delivered anything, so this is exactly the round that must not be allowed to report itself done")
	assert.Nil(t, d.submitted)
}
