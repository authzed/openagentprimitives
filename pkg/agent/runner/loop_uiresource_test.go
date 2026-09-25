package runner

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

// lastSessionViewOffer decodes the most recently published envelope's payload
// as a SessionViewOfferPayload, asserting the envelope itself is a
// well-formed KindSessionViewOffer. Mirrors fakePublisher.lastEcho.
func (f *fakePublisher) lastSessionViewOffer(t *testing.T) channelevents.SessionViewOfferPayload {
	t.Helper()
	require.NotEmpty(t, f.payloads, "no envelope was published")
	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(f.payloads[len(f.payloads)-1], &env), "unmarshal envelope")
	assert.Equal(t, channelevents.KindSessionViewOffer, env.Kind, "published envelope must be a session_view_offer")
	var p channelevents.SessionViewOfferPayload
	require.NoError(t, json.Unmarshal(env.Payload, &p), "unmarshal session_view_offer payload")
	return p
}

func TestApplyUIResource_PublishesOfferAndClearsField(t *testing.T) {
	pub := newFakePublisher(t)
	escalated := false
	in := tool.Result{
		Content:    "Widget summary the LLM already saw",
		UIResource: &tool.UIResourceSpec{URI: "ui://widget/1", HTML: []byte("<html></html>")},
	}

	out := applyUIResource(context.Background(), uiResourceDeps{}, pub.Publish, nil, "default", "sess-1", "", &escalated, in)

	assert.Nil(t, out.UIResource, "UIResource must be cleared so it never reaches a later dispatch")
	assert.Equal(t, "Widget summary the LLM already saw", out.Content, "Content (the Plan-1 summary) must be untouched")
	require.Equal(t, 1, pub.count(), "exactly one envelope must be published (no persistence backend wired, so no widget_offer — only session_view_offer)")

	offer := pub.lastSessionViewOffer(t)
	assert.Equal(t, "default/sess-1", offer.SessionRef)
	assert.True(t, escalated, "dedupe flag must be set after a successful publish")
}

func TestApplyUIResource_SecondWidgetSameWindowIsDeduped(t *testing.T) {
	pub := newFakePublisher(t)
	escalated := false

	first := tool.Result{UIResource: &tool.UIResourceSpec{URI: "ui://widget/1"}}
	applyUIResource(context.Background(), uiResourceDeps{}, pub.Publish, nil, "default", "sess-1", "", &escalated, first)
	require.Equal(t, 1, pub.count(), "first widget must publish the offer")

	second := tool.Result{UIResource: &tool.UIResourceSpec{URI: "ui://widget/2"}}
	out := applyUIResource(context.Background(), uiResourceDeps{}, pub.Publish, nil, "default", "sess-1", "", &escalated, second)

	assert.Equal(t, 1, pub.count(), "a second widget in the same window must NOT republish (soft-mute v1)")
	assert.Nil(t, out.UIResource, "the second result's UIResource must still be cleared even when deduped")
}

func TestApplyUIResource_NilUIResourceIsNoop(t *testing.T) {
	pub := newFakePublisher(t)
	escalated := false
	in := tool.Result{Content: "ordinary tool output"}

	out := applyUIResource(context.Background(), uiResourceDeps{}, pub.Publish, nil, "default", "sess-1", "", &escalated, in)

	assert.Equal(t, in, out, "a result without a UIResource must pass through unchanged")
	assert.Zero(t, pub.count(), "no publish for a result without a UIResource")
	assert.False(t, escalated)
}

func TestApplyUIResource_NoPublisherWiredSkipsWithoutPanic(t *testing.T) {
	escalated := false
	in := tool.Result{UIResource: &tool.UIResourceSpec{URI: "ui://widget/1"}}

	var out tool.Result
	assert.NotPanics(t, func() {
		out = applyUIResource(context.Background(), uiResourceDeps{}, nil, nil, "default", "sess-1", "", &escalated, in)
	}, "a nil publish func must be handled (logged), never dereferenced")
	assert.Nil(t, out.UIResource, "UIResource must still be cleared even when there's no publisher")
	assert.False(t, escalated, "no successful publish happened, so the dedupe flag must not be set")
}

// --- persistence tests (CR create+poll+finalize, status ref, widget_offer) ---

// recordingWidgetStatusWriter is a minimal uiResourceStatusWriter stub that
// records the ref it was called with. Mirrors loop_secretout_test.go's
// recordingStatusWriter.
type recordingWidgetStatusWriter struct {
	called bool
	ref    spiceboxv1alpha1.WidgetRef
}

func (w *recordingWidgetStatusWriter) AppendActiveWidget(_ context.Context, ref spiceboxv1alpha1.WidgetRef) error {
	w.called = true
	w.ref = ref
	return nil
}

func uiResourceTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s), "corev1.AddToScheme")
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s), "spiceboxv1alpha1.AddToScheme")
	return s
}

func uiResourceTestArtifactsService() *artifacts.Service {
	return artifacts.NewService(memory.NewLocal(inmem.NewBackend()), nil)
}

// readyOnCreateInterceptor flips a created ArtifactRender straight to Ready
// (no status subresource on the fake client here, so Create persists the
// status the poll loop reads) — mirrors artifact_prepare_test.go's
// readyOnCreate.
func readyOnCreateInterceptor() interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c k8sclient.WithWatch, obj k8sclient.Object, opts ...k8sclient.CreateOption) error {
			if ar, ok := obj.(*spiceboxv1alpha1.ArtifactRender); ok {
				ar.UID = types.UID("cr-uid-1")
				ar.Status.Phase = spiceboxv1alpha1.ArtifactRenderPhaseReady
				ar.Status.OutputRef = "mem://widget-out"
				ar.Status.OutputMIME = "text/html"
				ar.Status.OutputSize = int64(len("<html><script>alert(1)</script></html>"))
				ar.Status.OutputFilename = "widget.html"
			}
			return c.Create(ctx, obj, opts...)
		},
	}
}

// failCreateInterceptor makes every ArtifactRender Create fail, simulating a
// CR-create error (e.g. the operator's admission/validation rejecting it).
func failCreateInterceptor(errMsg string) interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c k8sclient.WithWatch, obj k8sclient.Object, opts ...k8sclient.CreateOption) error {
			if _, ok := obj.(*spiceboxv1alpha1.ArtifactRender); ok {
				return errors.New(errMsg)
			}
			return c.Create(ctx, obj, opts...)
		},
	}
}

func TestApplyUIResource_PersistsWidget_AppendsStatusRef_PublishesWidgetOffer(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(uiResourceTestScheme(t)).
		WithInterceptorFuncs(readyOnCreateInterceptor()).Build()
	svc := uiResourceTestArtifactsService()
	sw := &recordingWidgetStatusWriter{}
	pub := newFakePublisher(t)
	escalated := false

	const widgetHTML = "<html><script>alert(1)</script></html>"
	in := tool.Result{
		Content: "Widget summary the LLM already saw",
		UIResource: &tool.UIResourceSpec{
			URI: "ui://widget/1", HTML: []byte(widgetHTML), Tool: "mcpserver/widgets.render_form",
		},
	}

	ctx := memory.WithSystemApproval(context.Background(), "test")
	deps := uiResourceDeps{Client: c, Artifacts: svc, Status: sw, PollInterval: time.Millisecond}
	out := applyUIResource(ctx, deps, pub.Publish, nil, "default", "sess-1", types.UID("sess-uid-1"), &escalated, in)

	assert.Nil(t, out.UIResource, "UIResource must be cleared")
	assert.Equal(t, "Widget summary the LLM already saw", out.Content)

	// The ArtifactRender CR was created with Kind "mcpui" and the widget's
	// raw HTML payload — mirrors artifact_prepare's CR shape.
	var crList spiceboxv1alpha1.ArtifactRenderList
	require.NoError(t, c.List(ctx, &crList, k8sclient.InNamespace("default")))
	require.Len(t, crList.Items, 1, "exactly one ArtifactRender CR must be created")
	cr := crList.Items[0]
	assert.Equal(t, "mcpui", cr.Spec.Kind)
	assert.Equal(t, widgetHTML, string(cr.Spec.Payload))
	require.Len(t, cr.OwnerReferences, 1, "owner reference must be set when a session UID is given")
	assert.Equal(t, "AgentSession", cr.OwnerReferences[0].Kind)
	assert.Equal(t, types.UID("sess-uid-1"), cr.OwnerReferences[0].UID)

	// The durable status ref was appended.
	require.True(t, sw.called, "AppendActiveWidget must be called on successful persistence")
	assert.NotEmpty(t, sw.ref.ArtifactID)
	assert.Equal(t, "mcpserver/widgets.render_form", sw.ref.Tool)
	assert.Equal(t, "mcpui", sw.ref.RendererKind)

	// A widget_offer was published carrying the same ArtifactID, AND the
	// existing session_view_offer anchor was still published.
	require.Equal(t, 2, pub.count(), "both widget_offer and session_view_offer must be published")

	var widgetEnv channelevents.Envelope
	require.NoError(t, json.Unmarshal(pub.payloads[0], &widgetEnv))
	assert.Equal(t, channelevents.KindWidgetOffer, widgetEnv.Kind)
	var widgetPayload channelevents.WidgetOfferPayload
	require.NoError(t, json.Unmarshal(widgetEnv.Payload, &widgetPayload))
	assert.Equal(t, sw.ref.ArtifactID, widgetPayload.ArtifactID)
	assert.Equal(t, "mcpserver/widgets.render_form", widgetPayload.Tool)
	assert.Equal(t, "mcpui", widgetPayload.RendererKind)

	offer := pub.lastSessionViewOffer(t)
	assert.Equal(t, "default/sess-1", offer.SessionRef)
	assert.True(t, escalated)
}

// TestApplyUIResource_PersistsWidget_ParsesMetaCSP_SetsSpecCSP proves
// persistWidget carries a widget's declared `_meta.ui.csp` (mcp-ui / MCP
// Apps protocol — see pkg/tools/mcp/probe's real _meta shape) onto the created
// ArtifactRender CR's Spec.CSP, so it survives to FinalizeRevision.
func TestApplyUIResource_PersistsWidget_ParsesMetaCSP_SetsSpecCSP(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(uiResourceTestScheme(t)).
		WithInterceptorFuncs(readyOnCreateInterceptor()).Build()
	svc := uiResourceTestArtifactsService()
	sw := &recordingWidgetStatusWriter{}
	pub := newFakePublisher(t)
	escalated := false

	in := tool.Result{
		UIResource: &tool.UIResourceSpec{
			URI:  "ui://widget/1",
			HTML: []byte("<html></html>"),
			Meta: json.RawMessage(`{"ui":{"csp":{"connectDomains":["https://api.example.test"],"resourceDomains":["https://cdn.example.test"],"frameDomains":["https://embed.example.test"]}}}`),
		},
	}

	ctx := memory.WithSystemApproval(context.Background(), "test")
	deps := uiResourceDeps{Client: c, Artifacts: svc, Status: sw, PollInterval: time.Millisecond}
	applyUIResource(ctx, deps, pub.Publish, nil, "default", "sess-1", "", &escalated, in)

	var crList spiceboxv1alpha1.ArtifactRenderList
	require.NoError(t, c.List(ctx, &crList, k8sclient.InNamespace("default")))
	require.Len(t, crList.Items, 1)
	cr := crList.Items[0]
	require.NotNil(t, cr.Spec.CSP, "a declared _meta.ui.csp must be parsed onto Spec.CSP")
	assert.Equal(t, []string{"https://api.example.test"}, cr.Spec.CSP.ConnectDomains)
	assert.Equal(t, []string{"https://cdn.example.test"}, cr.Spec.CSP.ResourceDomains)
	assert.Equal(t, []string{"https://embed.example.test"}, cr.Spec.CSP.FrameDomains)
}

// TestApplyUIResource_PersistsWidget_MalformedMeta_NilCSP_NoError proves the
// safe-fallback contract: a malformed or absent `_meta` must never fail
// persistence — it degrades to Spec.CSP == nil (buildWidgetCSP's
// restrictive default), never an error.
func TestApplyUIResource_PersistsWidget_MalformedMeta_NilCSP_NoError(t *testing.T) {
	cases := []struct {
		name string
		meta json.RawMessage
	}{
		{name: "absent Meta", meta: nil},
		{name: "malformed (not JSON)", meta: json.RawMessage(`not json`)},
		{name: "well-formed JSON, wrong shape", meta: json.RawMessage(`{"ui":"not-an-object"}`)},
		{name: "empty object", meta: json.RawMessage(`{}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(uiResourceTestScheme(t)).
				WithInterceptorFuncs(readyOnCreateInterceptor()).Build()
			svc := uiResourceTestArtifactsService()
			sw := &recordingWidgetStatusWriter{}
			pub := newFakePublisher(t)
			escalated := false

			in := tool.Result{
				UIResource: &tool.UIResourceSpec{URI: "ui://widget/1", HTML: []byte("<html></html>"), Meta: tc.meta},
			}

			ctx := memory.WithSystemApproval(context.Background(), "test")
			deps := uiResourceDeps{Client: c, Artifacts: svc, Status: sw, PollInterval: time.Millisecond}
			out := applyUIResource(ctx, deps, pub.Publish, nil, "default", "sess-1", "", &escalated, in)

			assert.Nil(t, out.UIResource, "UIResource must still be cleared")
			require.True(t, sw.called, "persistence must still succeed despite malformed/absent _meta")

			var crList spiceboxv1alpha1.ArtifactRenderList
			require.NoError(t, c.List(ctx, &crList, k8sclient.InNamespace("default")))
			require.Len(t, crList.Items, 1)
			assert.Nil(t, crList.Items[0].Spec.CSP, "malformed/absent _meta must fall back to nil CSP, not an error")
		})
	}
}

func TestApplyUIResource_CRCreateFails_LogsClearsStillPublishesAnchor_NoWidgetOffer(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(uiResourceTestScheme(t)).
		WithInterceptorFuncs(failCreateInterceptor("admission rejected")).Build()
	svc := uiResourceTestArtifactsService()
	sw := &recordingWidgetStatusWriter{}
	pub := newFakePublisher(t)
	escalated := false

	in := tool.Result{
		Content:    "Widget summary the LLM already saw",
		UIResource: &tool.UIResourceSpec{URI: "ui://widget/1", HTML: []byte("<html></html>")},
	}

	ctx := memory.WithSystemApproval(context.Background(), "test")
	deps := uiResourceDeps{Client: c, Artifacts: svc, Status: sw, PollInterval: time.Millisecond}
	out := applyUIResource(ctx, deps, pub.Publish, nil, "default", "sess-1", "", &escalated, in)

	assert.Nil(t, out.UIResource, "UIResource must still be cleared on CR-create failure")
	assert.False(t, sw.called, "status ref must NOT be recorded when persistence failed")

	// Only the session_view_offer anchor was published — no widget_offer.
	require.Equal(t, 1, pub.count(), "CR failure must not publish a widget_offer, but must still publish the anchor")
	offer := pub.lastSessionViewOffer(t)
	assert.Equal(t, "default/sess-1", offer.SessionRef)
	assert.True(t, escalated, "the anchor publish must still succeed and set the dedupe flag")
}

func TestApplyUIResource_NilClientSkipsPersistence_StillPublishesAnchor(t *testing.T) {
	sw := &recordingWidgetStatusWriter{}
	pub := newFakePublisher(t)
	escalated := false

	in := tool.Result{UIResource: &tool.UIResourceSpec{URI: "ui://widget/1", HTML: []byte("<html></html>")}}

	// Client/Artifacts both nil — degrade path guarded before any k8s call.
	deps := uiResourceDeps{Status: sw}
	out := applyUIResource(context.Background(), deps, pub.Publish, nil, "default", "sess-1", "", &escalated, in)

	assert.Nil(t, out.UIResource)
	assert.False(t, sw.called, "no persistence backend wired ⇒ no status write")
	require.Equal(t, 1, pub.count(), "only the anchor, no widget_offer")
	assert.True(t, escalated)
}
