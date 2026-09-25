package meta_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake" // a registered kind with NO live-view surface
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

// offerViewReport records one Ready html revision and returns the service plus
// the artifact id the tool is called with — the shape a review agent reaches
// artifact_offer_view with, having just rendered its report.
func offerViewReport(t *testing.T) (*artifacts.Service, string) {
	t.Helper()
	svc := artifacts.NewService(memory.NewLocal(inmem.NewBackend()), nil)
	scope := memory.Scope{Kind: "session", ID: "default/sess1"}
	head := svc.NewArtifactID()
	cr := &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ar-report", Namespace: "default", UID: types.UID("u-report"),
			Labels: map[string]string{artifacts.LabelArtifactID: head},
		},
		Spec: spiceboxv1alpha1.ArtifactRenderSpec{Kind: "html"},
		Status: spiceboxv1alpha1.ArtifactRenderStatus{
			Phase: spiceboxv1alpha1.ArtifactRenderPhaseReady, OutputMIME: "text/html",
		},
	}
	_, err := svc.FinalizeRevision(memory.WithSystemApproval(context.Background(), "test"), scope, cr)
	require.NoError(t, err, "finalize the report revision")
	return svc, head
}

// offerViewSession is the session context every case here calls with.
func offerViewSession() *tool.SessionContext {
	return &tool.SessionContext{Namespace: "default", Name: "sess1"}
}

// TestArtifactOfferView_NoLiveViewSurface_SaysSoAndDoesNotFailTheRound covers
// the case the owner's ruling turns on: a channel with no live-view surface.
//
// The outbound relay drops an envelope whose sub-channel sender is nil with
// nothing but a log line, so publishing to such a channel and reporting an
// offer "sent" tells the model a button is waiting for a user who can never be
// shown one. It is still not a FAILURE — the attachment is the delivery on
// every channel — so the round must go on.
func TestArtifactOfferView_NoLiveViewSurface_SaysSoAndDoesNotFailTheRound(t *testing.T) {
	svc, head := offerViewReport(t)

	published := 0
	tl := meta.NewArtifactOfferView(meta.ArtifactOfferViewConfig{
		Artifacts: svc, AvailableKinds: []string{"html"},
		ChannelKind:       "fake",
		NATSPublish:       func(context.Context, string, []byte) error { published++; return nil },
		NATSSubjectPrefix: "ap.session.default.sess1",
	})

	args, err := json.Marshal(map[string]any{"artifact_id": head})
	require.NoError(t, err)
	res, execErr := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, offerViewSession())
	require.NoError(t, execErr)

	assert.Zero(t, published,
		"nothing on this channel could render the offer, so putting it on the wire only manufactures the false success")
	assert.False(t, res.IsError,
		"a channel that cannot render a live view is expected here, not a failure — refusing would make such channels worse off than before")
	assert.False(t, res.Terminal, "and it must not end the round")

	low := strings.ToLower(res.Content)
	assert.Contains(t, low, "fake", "name the transport, so the model knows the limit is its channel's and not its call's")
	assert.Contains(t, low, "attach", "and point it at the delivery that does work on every channel")
	assert.NotContains(t, low, "can click",
		"nothing was rendered, so there is nothing for the user to click")
}

// TestArtifactOfferView_PublishedOfferNeverReportsItselfDelivered pins the
// honest wording on the success path.
//
// This tool publishes and stops: the render happens in another process, and no
// result ever comes back. So the strongest true statement is that the offer
// went out — and the model, which repeats what it is told here to the user,
// must be pointed at the attachment as the copy the user is guaranteed to have.
func TestArtifactOfferView_PublishedOfferNeverReportsItselfDelivered(t *testing.T) {
	svc, head := offerViewReport(t)

	published := 0
	tl := meta.NewArtifactOfferView(meta.ArtifactOfferViewConfig{
		Artifacts: svc, AvailableKinds: []string{"html"},
		ChannelKind:       "slack",
		NATSPublish:       func(context.Context, string, []byte) error { published++; return nil },
		NATSSubjectPrefix: "ap.session.default.sess1",
	})

	args, err := json.Marshal(map[string]any{"artifact_id": head})
	require.NoError(t, err)
	res, execErr := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, offerViewSession())
	require.NoError(t, execErr)

	require.Equal(t, 1, published, "slack renders live-view offers, so the envelope must go out")
	assert.False(t, res.IsError, "content: %s", res.Content)

	low := strings.ToLower(res.Content)
	assert.Contains(t, low, "attach",
		"the attachment is what actually guarantees the user has the artifact; the model has to be told so here")
	assert.NotContains(t, low, "offer sent — the user can click to open it",
		"the render is never observed by this tool, so it must not be stated as fact")
}

// TestArtifactOfferView_PublishFailure_ReportsThatNothingWasSent is the third
// outcome. Unlike a channel with no surface, this one IS a failure: the offer
// was meant to go out on a channel that would have rendered it, and did not.
func TestArtifactOfferView_PublishFailure_ReportsThatNothingWasSent(t *testing.T) {
	svc, head := offerViewReport(t)

	tl := meta.NewArtifactOfferView(meta.ArtifactOfferViewConfig{
		Artifacts: svc, AvailableKinds: []string{"html"},
		ChannelKind: "slack",
		NATSPublish: func(context.Context, string, []byte) error {
			return assert.AnError
		},
		NATSSubjectPrefix: "ap.session.default.sess1",
	})

	// The publish path retries against a ten-second budget; a short deadline
	// ends it at the first backoff instead of holding the suite for ten
	// seconds. Everything before the publish is in-memory and completes well
	// inside it.
	ctx, cancel := context.WithTimeout(
		memory.WithSystemApproval(context.Background(), "test"), 250*time.Millisecond)
	t.Cleanup(cancel)

	args, err := json.Marshal(map[string]any{"artifact_id": head})
	require.NoError(t, err)
	res, execErr := tl.Execute(ctx, args, offerViewSession())
	require.NoError(t, execErr, "a failed publish is a tool result the model can act on, not a Go error")

	assert.True(t, res.IsError, "an offer that never left the runner is a failure the model must see")
	low := strings.ToLower(res.Content)
	assert.Contains(t, low, "not", "the result has to say the offer did NOT go out")
	assert.Contains(t, low, "attach", "and send the model to the delivery that still works")
}

// TestArtifactOfferView_ThreeOutcomesAreDistinguishable is the property the
// whole change exists for: a model reading only the tool result can tell which
// of the three happened. Asserted as mutual difference so no two collapse onto
// one another as the wording is edited.
func TestArtifactOfferView_ThreeOutcomesAreDistinguishable(t *testing.T) {
	run := func(t *testing.T, channelKind string, publish func(context.Context, string, []byte) error, ctx context.Context) tool.Result {
		t.Helper()
		svc, head := offerViewReport(t)
		tl := meta.NewArtifactOfferView(meta.ArtifactOfferViewConfig{
			Artifacts: svc, AvailableKinds: []string{"html"},
			ChannelKind: channelKind, NATSPublish: publish,
			NATSSubjectPrefix: "ap.session.default.sess1",
		})
		args, err := json.Marshal(map[string]any{"artifact_id": head})
		require.NoError(t, err)
		res, execErr := tl.Execute(ctx, args, offerViewSession())
		require.NoError(t, execErr)
		return res
	}

	base := memory.WithSystemApproval(context.Background(), "test")
	failCtx, cancel := context.WithTimeout(base, 250*time.Millisecond)
	t.Cleanup(cancel)

	ok := func(context.Context, string, []byte) error { return nil }
	sent := run(t, "slack", ok, base)
	skipped := run(t, "fake", ok, base)
	failed := run(t, "slack", func(context.Context, string, []byte) error { return assert.AnError }, failCtx)

	assert.NotEqual(t, sent.Content, skipped.Content, "sent and skipped must not read alike")
	assert.NotEqual(t, sent.Content, failed.Content, "sent and failed must not read alike")
	assert.NotEqual(t, skipped.Content, failed.Content, "skipped and failed must not read alike")

	// And the one machine-readable bit has to split them the same way: only the
	// failure is an error, so a channel that simply cannot render one never
	// ends the round.
	assert.False(t, sent.IsError)
	assert.False(t, skipped.IsError)
	assert.True(t, failed.IsError)
}
