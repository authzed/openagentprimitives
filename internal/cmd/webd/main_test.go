package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	natsserver "github.com/nats-io/nats-server/v2/server"
	natstest "github.com/nats-io/nats-server/v2/test"
	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/platform/identityd"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/agentui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/artifactview"
	"github.com/authzed/openagentprimitives/pkg/web/webui/chatembed"
	"github.com/authzed/openagentprimitives/pkg/web/webui/contenttoken"
	"github.com/authzed/openagentprimitives/pkg/web/webui/interact"
	"github.com/authzed/openagentprimitives/pkg/web/webui/registry"
	"github.com/authzed/openagentprimitives/pkg/web/webui/sessions"
	"github.com/authzed/openagentprimitives/pkg/web/webui/sessionview"
	"github.com/authzed/openagentprimitives/pkg/web/webui/webassets"
)

// var _ interact.Deps = (*artifactViewDeps)(nil) is a compile-time proof that
// webd's viewer-capable umbrella satisfies the /interact plugin's dependency
// surface — a missing method or a signature drift (e.g. CheckInteract's
// fullyConsistent bool) fails the build, not a runtime cast in production.
var _ interact.Deps = (*artifactViewDeps)(nil)

// var _ sessionview.Deps = (*artifactViewDeps)(nil) is the same compile-time
// proof for the session-view plugin: webd's viewer-capable umbrella already
// exposes CheckInteract/OperatorURL/MemoryToken/NATS/TrustedOrigin/Logger for
// artifactview + interact, and this line locks that coincidence in so a
// future signature drift on either side fails the build here, not a runtime
// cast in production.
var _ sessionview.Deps = (*artifactViewDeps)(nil)

// var _ chatembed.Deps = (*artifactViewDeps)(nil) is the same compile-time
// proof for the chat-embed plugin: it needs only CheckInteract + Logger, both
// already exposed by the viewer-capable umbrella for interact/sessionview
// above, so this line locks that coincidence in rather than leaving it to a
// runtime deps.(chatembed.Deps) cast.
var _ chatembed.Deps = (*artifactViewDeps)(nil)

// The agentui.Deps compile-time guard (var _ agentui.Deps =
// (*artifactViewDeps)(nil)) lives in main.go itself, not here: a guard in a
// _test.go file is invisible to `go build ./...` and only ever caught by
// `go vet`/`go test`, which contradicts the "deps drift is a build error"
// property it exists to provide. See main.go's copy (next to the
// artifactViewDeps struct it certifies) for the doc comment.

// TestFetchRenderBytes covers the artifact-bytes fetch path: it must set the
// bearer, return body + content-type on 200, and propagate non-200 as an
// error (never silently drop).
func TestFetchRenderBytes(t *testing.T) {
	cases := []struct {
		name      string
		handler   http.HandlerFunc
		wantErr   bool
		wantBody  string
		wantMIME  string
		wantInErr string
	}{
		{
			name: "200 with content-type: bytes + mime returned, bearer sent",
			handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer tok-123", r.Header.Get("Authorization"))
				assert.Equal(t, "/artifact/ns1/sess1/ar-abc/output", r.URL.Path)
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write([]byte("<h1>hi</h1>"))
			},
			wantBody: "<h1>hi</h1>",
			wantMIME: "text/html; charset=utf-8",
		},
		{
			name: "404 from operator: error propagated with status",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "not found", http.StatusNotFound)
			},
			wantErr:   true,
			wantInErr: "status=404",
		},
		{
			name: "401 unauthorized: error propagated with status",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			},
			wantErr:   true,
			wantInErr: "status=401",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)

			body, mime, err := fetchRenderBytes(context.Background(), srv.URL, "tok-123", "ns1", "sess1", "ar-abc")
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantInErr)
				assert.Nil(t, body)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantBody, string(body))
			assert.Equal(t, tc.wantMIME, mime)
		})
	}
}

// TestFetchBundleBytes covers the download path's fetch: it must hit the
// bundle route (not /artifact/…/output), set the bearer, and pass through
// whatever the operator returns — including a zipped response — verbatim.
func TestFetchBundleBytes(t *testing.T) {
	cases := []struct {
		name      string
		handler   http.HandlerFunc
		wantErr   bool
		wantBody  string
		wantMIME  string
		wantInErr string
	}{
		{
			name: "200 raw html: bytes + mime returned, bearer sent, bundle path used",
			handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer tok-123", r.Header.Get("Authorization"))
				assert.Equal(t, "/artifact-bundle/ns1/sess1/ar-abc/bundle", r.URL.Path)
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write([]byte("<h1>hi</h1>"))
			},
			wantBody: "<h1>hi</h1>",
			wantMIME: "text/html; charset=utf-8",
		},
		{
			name: "200 zip: bytes + application/zip returned unchanged",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/zip")
				_, _ = w.Write([]byte("PK\x03\x04fake-zip"))
			},
			wantBody: "PK\x03\x04fake-zip",
			wantMIME: "application/zip",
		},
		{
			name: "404 from operator: error propagated with status",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "not found", http.StatusNotFound)
			},
			wantErr:   true,
			wantInErr: "status=404",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)

			body, mime, err := fetchBundleBytes(context.Background(), srv.URL, "tok-123", "ns1", "sess1", "ar-abc")
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantInErr)
				assert.Nil(t, body)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantBody, string(body))
			assert.Equal(t, tc.wantMIME, mime)
		})
	}
}

// renderedCRKind builds a Ready ArtifactRender CR of the given renderer kind so
// the webd ContentRender redirect can be exercised against a real
// artifacts.Service. Mirrors pkg/platform/artifacts' test factory but lets the caller
// pick the kind (html = standalone, svg = bundled-only).
func renderedCRKind(t *testing.T, name, headID, kind string, internal bool, uid types.UID) *spiceboxv1alpha1.ArtifactRender {
	t.Helper()
	cr := &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", UID: uid,
			Labels:      map[string]string{artifacts.LabelArtifactID: headID},
			Annotations: map[string]string{artifacts.AnnoChangeDescription: "x"},
		},
		Spec: spiceboxv1alpha1.ArtifactRenderSpec{Kind: kind},
		Status: spiceboxv1alpha1.ArtifactRenderStatus{
			Phase:     spiceboxv1alpha1.ArtifactRenderPhaseReady,
			OutputRef: "mem://" + name, OutputMIME: "text/html", OutputSize: 10, OutputFilename: name + ".out",
		},
	}
	if internal {
		cr.Annotations[artifacts.AnnoInternal] = "true"
	}
	return cr
}

// TestArtifactViewDeps_ContentRender_RedirectsBundledOnlyToPreviewChild proves
// the webd ContentRender redirect: a bundled-only (svg) artifact frames its
// internal html preview child render (and is not-ready while that child is still
// generating), while a standalone (html) artifact frames its newest render.
// Exercises the real artifacts.Service + the channelassets registry (svg/css/html
// are blank-imported by this binary).
func TestArtifactViewDeps_ContentRender_RedirectsBundledOnlyToPreviewChild(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	svc := artifacts.NewService(memory.NewLocal(inmem.NewBackend()), nil)
	d := &artifactViewDeps{artSvc: svc}
	const ns, sess = "default", "sess1"
	scope := memory.Scope{Kind: "session", ID: ns + "/" + sess}

	t.Run("standalone html: frames newest render, ready=true", func(t *testing.T) {
		headID := svc.NewArtifactID()
		_, err := svc.FinalizeRevision(ctx, scope, renderedCRKind(t, "ar-html", headID, "html", false, types.UID("uid-html")))
		require.NoError(t, err, "finalize html revision")

		rn, ready, err := d.ContentRender(ctx, ns, sess, headID)
		require.NoError(t, err)
		assert.True(t, ready, "standalone artifact is immediately servable")
		assert.Equal(t, "ar-html", rn, "standalone frames its newest render")
	})

	t.Run("bundled-only svg, no preview yet: ready=false, no render", func(t *testing.T) {
		headID := svc.NewArtifactID()
		_, err := svc.FinalizeRevision(ctx, scope, renderedCRKind(t, "ar-svg", headID, "svg", false, types.UID("uid-svg")))
		require.NoError(t, err, "finalize svg revision")

		rn, ready, err := d.ContentRender(ctx, ns, sess, headID)
		require.NoError(t, err)
		assert.False(t, ready, "bundled-only artifact is not servable until its preview child lands")
		assert.Empty(t, rn, "raw svg render is never framed")
	})

	t.Run("bundled-only svg, preview child landed: frames the child render", func(t *testing.T) {
		headID := svc.NewArtifactID()
		srcRev, err := svc.FinalizeRevision(ctx, scope, renderedCRKind(t, "ar-svg2", headID, "svg", false, types.UID("uid-svg2")))
		require.NoError(t, err, "finalize svg revision")

		// The internal html preview child for the latest source revision.
		childID := svc.PreviewChildID(srcRev.RevisionID)
		_, err = svc.FinalizeRevision(ctx, scope, renderedCRKind(t, "ar-svg2-preview", childID, "html", true, types.UID("uid-svg2-preview")))
		require.NoError(t, err, "finalize preview child")

		rn, ready, err := d.ContentRender(ctx, ns, sess, headID)
		require.NoError(t, err)
		assert.True(t, ready, "bundled-only artifact is servable once its preview child lands")
		assert.Equal(t, "ar-svg2-preview", rn, "bundled-only frames the internal preview child render, never raw svg")
	})

	t.Run("missing head: ready=false, no error", func(t *testing.T) {
		rn, ready, err := d.ContentRender(ctx, ns, sess, "artifact-nope")
		require.NoError(t, err)
		assert.False(t, ready)
		assert.Empty(t, rn)
	})
}

// TestArtifactViewDeps_FetchWidget_BypassesBundledOnlyRedirect proves
// FetchWidget resolves an mcpui artifact's render DIRECTLY via
// artSvc.ResolveToRender, never through ContentRender/kindIsBundledOnly —
// mcpui is classified DeliveryBundledOnly (so the generic artifact-view
// never frames its raw bytes) but registers no PreviewComposer, so routing a
// widget fetch through ContentRender's bundled-only branch would look for a
// preview child that never exists and the widget would never be servable.
func TestArtifactViewDeps_FetchWidget_BypassesBundledOnlyRedirect(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	svc := artifacts.NewService(memory.NewLocal(inmem.NewBackend()), nil)
	const ns, sess = "default", "sess1"
	scope := memory.Scope{Kind: "session", ID: ns + "/" + sess}

	headID := svc.NewArtifactID()
	_, err := svc.FinalizeRevision(ctx, scope, renderedCRKind(t, "ar-widget1", headID, "mcpui", false, types.UID("uid-widget1")))
	require.NoError(t, err, "finalize mcpui revision")

	var gotPath string
	opSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><script>run()</script></html>`))
	}))
	defer opSrv.Close()

	d := &artifactViewDeps{artSvc: svc, operatorURL: opSrv.URL, token: "tok-123"}
	out, meta, err := d.FetchWidget(ctx, ns, sess, headID)
	require.NoError(t, err)
	assert.Equal(t, `<html><script>run()</script></html>`, string(out), "widget bytes are fetched verbatim")
	assert.Nil(t, meta.CSP, "no _meta.ui.csp was declared on this revision, so CSP stays nil (the safe restrictive-default fallback)")
	assert.Equal(t, "/artifact/default/sess1/ar-widget1/output", gotPath, "FetchWidget must resolve to the render's own CR name, not a preview child")
}

// TestArtifactViewDeps_FetchWidget_ThreadsDeclaredCSP proves the CSP-
// threading path: a widget revision finalized with Spec.CSP set (mirroring
// pkg/agent/runner/loop.go's persistWidget parsing `_meta.ui.csp`) must have
// that CSP resolved by ResolveWidgetCSP and mapped into WidgetMeta.CSP — the
// consumer end of the same producer→consumer contract
// TestFinalizeRevision_ResolveWidgetCSP_RoundTrips proves at the
// artifacts.Service layer.
func TestArtifactViewDeps_FetchWidget_ThreadsDeclaredCSP(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	svc := artifacts.NewService(memory.NewLocal(inmem.NewBackend()), nil)
	const ns, sess = "default", "sess1"
	scope := memory.Scope{Kind: "session", ID: ns + "/" + sess}

	headID := svc.NewArtifactID()
	cr := renderedCRKind(t, "ar-widget2", headID, "mcpui", false, types.UID("uid-widget2"))
	cr.Spec.CSP = &spiceboxv1alpha1.WidgetCSP{
		ConnectDomains:  []string{"https://api.example.test"},
		ResourceDomains: []string{"https://cdn.example.test"},
		FrameDomains:    []string{"https://embed.example.test"},
	}
	_, err := svc.FinalizeRevision(ctx, scope, cr)
	require.NoError(t, err, "finalize mcpui revision with a declared CSP")

	opSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html></html>`))
	}))
	defer opSrv.Close()

	d := &artifactViewDeps{artSvc: svc, operatorURL: opSrv.URL, token: "tok-123"}
	_, meta, err := d.FetchWidget(ctx, ns, sess, headID)
	require.NoError(t, err)
	require.NotNil(t, meta.CSP, "a declared _meta.ui.csp must be threaded through, not dropped")
	assert.Equal(t, []string{"https://api.example.test"}, meta.CSP.ConnectDomains)
	assert.Equal(t, []string{"https://cdn.example.test"}, meta.CSP.ResourceDomains)
	assert.Equal(t, []string{"https://embed.example.test"}, meta.CSP.FrameDomains)
}

// TestArtifactViewDeps_FetchWidget_UnknownArtifact_Errors proves an unknown
// artifactID surfaces an error (never a silent empty body).
func TestArtifactViewDeps_FetchWidget_UnknownArtifact_Errors(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	svc := artifacts.NewService(memory.NewLocal(inmem.NewBackend()), nil)
	d := &artifactViewDeps{artSvc: svc}

	_, _, err := d.FetchWidget(ctx, "default", "sess1", "artifact-nope")
	assert.Error(t, err)
}

// TestArtifactViewDeps_ActiveWidgets_ReadsSessionStatus proves ActiveWidgets
// reads AgentSession.status.activeWidgets and maps each WidgetRef to the
// sessionview package-local DTO.
func TestArtifactViewDeps_ActiveWidgets_ReadsSessionStatus(t *testing.T) {
	const ns, sess = "default", "sess1"
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	as := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: sess},
		Status: spiceboxv1alpha1.AgentSessionStatus{
			ActiveWidgets: []spiceboxv1alpha1.WidgetRef{
				{ArtifactID: "artifact-w1", Tool: "render_chart", RendererKind: "mcpui"},
				{ArtifactID: "artifact-w2", Tool: "render_table", RendererKind: "mcpui"},
			},
		},
	}
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(as).Build()
	d := &artifactViewDeps{webdDeps: &webdDeps{k8s: k}, logger: logr.Discard()}

	got, err := d.ActiveWidgets(context.Background(), ns, sess)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, sessionview.WidgetRef{ArtifactID: "artifact-w1", Tool: "render_chart", RendererKind: "mcpui"}, got[0])
	assert.Equal(t, sessionview.WidgetRef{ArtifactID: "artifact-w2", Tool: "render_table", RendererKind: "mcpui"}, got[1])
}

// TestArtifactViewDeps_ActiveWidgets_MissingSession_Errors proves a Get
// failure (e.g. the session vanished) is returned, never swallowed into an
// empty list at this layer — page.go's activeWidgetProps is the layer that
// degrades it to "no widgets" for the bootstrap props.
func TestArtifactViewDeps_ActiveWidgets_MissingSession_Errors(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	k := fake.NewClientBuilder().WithScheme(scheme).Build()
	d := &artifactViewDeps{webdDeps: &webdDeps{k8s: k}, logger: logr.Discard()}

	_, err := d.ActiveWidgets(context.Background(), "default", "nope")
	assert.Error(t, err)
}

// TestArtifactViewDeps_WorkshopNamespacesFor_FiltersByOwnerAndReadiness proves
// the three conditions browserstart's dynamic arm depends on: only the
// SUBJECT's own workshops (StarterCanonical match), only Ready ones, and only
// those with a provisioned namespace — a Workshop still Provisioning has none
// yet and must not surface as startable.
func TestArtifactViewDeps_WorkshopNamespacesFor_FiltersByOwnerAndReadiness(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	mine := &spiceboxv1alpha1.Workshop{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "mine-ready"},
		Spec:       spiceboxv1alpha1.WorkshopSpec{StarterCanonical: "owner-a"},
		Status:     spiceboxv1alpha1.WorkshopStatus{Namespace: "ws-1", Phase: spiceboxv1alpha1.WorkshopPhaseReady},
	}
	mineNotReady := &spiceboxv1alpha1.Workshop{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "mine-provisioning"},
		Spec:       spiceboxv1alpha1.WorkshopSpec{StarterCanonical: "owner-a"},
		Status:     spiceboxv1alpha1.WorkshopStatus{Namespace: "", Phase: "Provisioning"},
	}
	someoneElses := &spiceboxv1alpha1.Workshop{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "not-mine"},
		Spec:       spiceboxv1alpha1.WorkshopSpec{StarterCanonical: "owner-b"},
		Status:     spiceboxv1alpha1.WorkshopStatus{Namespace: "ws-2", Phase: spiceboxv1alpha1.WorkshopPhaseReady},
	}
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mine, mineNotReady, someoneElses).Build()
	d := &artifactViewDeps{webdDeps: &webdDeps{k8s: k}, logger: logr.Discard()}

	got, err := d.WorkshopNamespacesFor(context.Background(), "user:owner-a")
	require.NoError(t, err)
	assert.Equal(t, []string{"ws-1"}, got,
		"only the Ready workshop this subject owns; the not-yet-provisioned one and the other owner's are both excluded")
}

// TestArtifactViewDeps_WorkshopNamespacesFor_EmptyCanonical_FailsClosed proves
// a bare "user:" subject — which CanonicalUserID() decodes without error, per
// identity.Subject's own doc — is refused rather than compared against
// Workshop.Spec.StarterCanonical, which is +optional and so could legitimately
// be unset on some other workshop; matching on "" would leak that one's
// namespace to whoever asks with no canonical id at all.
func TestArtifactViewDeps_WorkshopNamespacesFor_EmptyCanonical_FailsClosed(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	unowned := &spiceboxv1alpha1.Workshop{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "unowned"},
		Status:     spiceboxv1alpha1.WorkshopStatus{Namespace: "ws-3", Phase: spiceboxv1alpha1.WorkshopPhaseReady},
	}
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(unowned).Build()
	d := &artifactViewDeps{webdDeps: &webdDeps{k8s: k}, logger: logr.Discard()}

	got, err := d.WorkshopNamespacesFor(context.Background(), "user:")
	assert.ErrorIs(t, err, errEmptyWorkshopSubject)
	assert.Nil(t, got)
}

// TestArtifactViewDeps_SignVerifyWidgetToken_RoundTrip proves the widget
// content-token wiring: Ns/Sess/ArtifactID survive the round trip, and the
// token is Kind=widget (contenttoken.KindWidget) — never replayable at
// artifactview's VerifyContentToken/VerifyAssetToken.
func TestArtifactViewDeps_SignVerifyWidgetToken_RoundTrip(t *testing.T) {
	d := &artifactViewDeps{ctSigner: contenttoken.New(make([]byte, 32))}

	tok, err := d.SignWidgetToken("default", "sess1", "artifact-w1")
	require.NoError(t, err)

	ns, sess, artifactID, err := d.VerifyWidgetToken(tok)
	require.NoError(t, err)
	assert.Equal(t, "default", ns)
	assert.Equal(t, "sess1", sess)
	assert.Equal(t, "artifact-w1", artifactID)

	_, _, _, err = d.VerifyContentToken(tok)
	assert.Error(t, err, "a widget token must never be usable at VerifyContentToken")
	_, _, _, err = d.VerifyAssetToken(tok)
	assert.Error(t, err, "a widget token must never be usable at VerifyAssetToken")
}

// classWithSessionViewsCaps builds an AgentClass whose spec.capabilities key
// "session_views" carries the given raw JSON (nil ⇒ the key is absent
// entirely — no grant at all). Mirrors pkg/web/webui/interact/handlers_test.go's
// classWithSessionViews fixture, adapted to a real k8s object so it can be
// stored via the fake client (AgentClassOf reads it with a Get, not a func
// field).
func classWithSessionViewsCaps(t *testing.T, ns, name string, raw json.RawMessage) *spiceboxv1alpha1.AgentClass {
	t.Helper()
	ac := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	if raw != nil {
		ac.Spec.Capabilities = map[string]apiextensionsv1.JSON{"session_views": {Raw: raw}}
	}
	return ac
}

// TestArtifactViewDeps_SessionViews proves SessionViews reads the
// session_views grant from the AgentClass's SPEC (via agentcaps), never
// status, and degrades to nil (read-only viewer) on every failure mode:
// no capability, an explicitly-disabled capability, a granted-but-empty
// config, and an unresolvable AgentClass. Only an active grant with a
// non-empty interactions list returns anything.
func TestArtifactViewDeps_SessionViews(t *testing.T) {
	const ns, sess, class = "default", "sess1", "agent-class-1"
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))

	newDeps := func(objs ...client.Object) *artifactViewDeps {
		k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
		return &artifactViewDeps{webdDeps: &webdDeps{k8s: k}, logger: logr.Discard()}
	}
	as := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: sess},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: class},
	}

	cases := []struct {
		name string
		objs []client.Object
		want []string
	}{
		{
			name: "no capabilities map at all: nil (opt-in, absent ⇒ no interactions)",
			objs: []client.Object{as, classWithSessionViewsCaps(t, ns, class, nil)},
			want: nil,
		},
		{
			name: "session_views explicitly disabled: nil",
			objs: []client.Object{as, classWithSessionViewsCaps(t, ns, class, json.RawMessage(`{"enabled":false,"interactions":["user_message"]}`))},
			want: nil,
		},
		{
			name: "session_views granted but empty config: nil (session_views:{} is read-only)",
			objs: []client.Object{as, classWithSessionViewsCaps(t, ns, class, json.RawMessage(`{}`))},
			want: nil,
		},
		{
			name: "session_views granted with user_message: [\"user_message\"]",
			objs: []client.Object{as, classWithSessionViewsCaps(t, ns, class, json.RawMessage(`{"interactions":["user_message"]}`))},
			want: []string{"user_message"},
		},
		{
			name: "AgentClass unresolvable (session references a class that doesn't exist): nil, fail closed",
			objs: []client.Object{as}, // no AgentClass object at all
			want: nil,
		},
		{
			name: "AgentSession itself unresolvable: nil, fail closed",
			objs: []client.Object{classWithSessionViewsCaps(t, ns, class, json.RawMessage(`{"interactions":["user_message"]}`))}, // no AgentSession
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newDeps(tc.objs...)
			got := d.SessionViews(context.Background(), ns, sess)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestBuildArtifactViewDepsFailsClosed verifies the viewer is disabled (nil
// deps → routes 503) when its required collaborators are absent. SpiceDB is
// unconfigured here (no SPICEDB_ENDPOINT/TOKEN in env), so the function must
// return nil rather than a partially-wired, fail-open deps.
func TestBuildArtifactViewDepsFailsClosed(t *testing.T) {
	t.Setenv("SPICEDB_TOKEN", "")

	av := buildArtifactViewDeps(
		&webdDeps{}, // identity-only base; viewer extends it when configured
		nil,         // nc: NATS unused on this path
		"http://operator:8082",
		"/nonexistent/token",
		"",    // spicedbEndpoint: unconfigured → viewer disabled
		false, // spicedbInsecure
		"",    // spicedbTokenPath: no file; token falls back to SPICEDB_TOKEN (empty)
		[]byte("key"),
		nil, // cookieSigner unused on this path
		func() string { return "https://trusted.example.com" },
		func() string { return "https://sandbox.example.com" },
		logr.Logger{}, // zero-value: nil-sink no-op; tests need not configure a real logger
		"", "",        // admindURL, admindToken: unconfigured; adminui plugin fails closed
		[]string{"default"},      // startNamespaces: the flag's own default
		"agentprimitives-system", // accessTokenNamespace: unused on this fail-closed path
		2160*time.Hour,           // accessTokenLifetime: unused on this fail-closed path
	)
	assert.Nil(t, av, "viewer must fail closed when SpiceDB is unconfigured")
}

// TestInteractCheckArgs pins the two properties every session-scoped interact
// check in this binary is made with — the half of the ended-session start
// route's subset argument that lives HERE rather than in either web package.
//
// That argument (pkg/web/webui/agentui's startHandler) says its gate admits a
// strict subset of what the dashboard's fully-consistent derivation admits. It
// depends on this adapter binding fullyConsistent=true and deriving the same
// canonical id the dashboard's own gate derives. Neither is observable from
// CheckInteract itself: its collaborator is a concrete *spicedb.Client with no
// seam, which is exactly how a consistency flag gets flipped unnoticed.
func TestInteractCheckArgs(t *testing.T) {
	t.Run("a well-formed subject: the canonical id, checked fully consistently", func(t *testing.T) {
		display := "alice@example.com"
		subject := "user:" + base64.RawURLEncoding.EncodeToString([]byte(display))

		canonical, fullyConsistent, err := interactCheckArgs(subject)
		require.NoError(t, err)
		assert.True(t, fullyConsistent,
			"a snapshot read would admit a subject whose access was just revoked, and would break the "+
				"subset argument the ended-session start route rests on")
		assert.NotEmpty(t, canonical)
		assert.NotContains(t, canonical.String(), "user:",
			"the SpiceDB object id is the bare canonical, never the prefixed subject")
	})

	// The typed conversion is what makes this fail-closed. strings.TrimPrefix —
	// the form pkg/web/webui/sessions' list gate documents as a defect — would hand
	// SpiceDB "service:foo" as a USER id and check a subject nobody is.
	t.Run("a non-user subject: an error, never a silently-mangled id", func(t *testing.T) {
		_, _, err := interactCheckArgs("service:channelsd")
		require.Error(t, err, "an indeterminate subject must not be turned into an id and checked")
	})

	t.Run("an empty canonical: an error, never a check against a bare prefix", func(t *testing.T) {
		_, _, err := interactCheckArgs("user:")
		require.ErrorIs(t, err, errEmptyInteractSubject)
	})
}

// TestUmbrellaDepsCasts proves the fail-closed type-selection mechanism: the
// identity-only umbrella (*webdDeps) always casts to identityd.WebDeps but NOT
// to artifactview.Deps, interact.Deps, sessionview.Deps, agentui.Deps, or
// sessions.Deps (so the viewer, /interact, /session-view, /agent-ui, and
// /sessions routes are all absent when SpiceDB is unconfigured), while the
// viewer-capable umbrella (*artifactViewDeps) casts to ALL SIX (identityd
// hosted + viewer + /interact + /session-view + /agent-ui + /sessions served
// when fully configured). agentui.Deps and sessions.Deps both fail on the
// identity-only umbrella specifically because they lack CheckInteract — NOT
// because they lack K8s or (as of this fix) Logger, both of which *webdDeps
// does carry; see TestAgentUIRoutes_DegradedWebdDeps and
// TestSessionsRoutes_DegradedWebdDeps below for the runtime consequence of
// that specific gap.
func TestUmbrellaDepsCasts(t *testing.T) {
	t.Run("SpiceDB unconfigured: WebDeps cast succeeds, artifactview.Deps, interact.Deps, sessionview.Deps, agentui.Deps, sessions.Deps cast fail", func(t *testing.T) {
		var deps webui.Deps = &webdDeps{externalBaseURL: func() string { return "https://trusted.example" }}
		_, isWebDeps := deps.(identityd.WebDeps)
		assert.True(t, isWebDeps, "identity surface must mount (WebDeps cast succeeds)")
		_, isViewer := deps.(artifactview.Deps)
		assert.False(t, isViewer, "viewer must be absent (artifactview.Deps cast fails) when SpiceDB unconfigured")
		_, isInteract := deps.(interact.Deps)
		assert.False(t, isInteract, "/interact must be absent (interact.Deps cast fails) when SpiceDB unconfigured")
		_, isSessionView := deps.(sessionview.Deps)
		assert.False(t, isSessionView, "/session-view must be absent (sessionview.Deps cast fails) when SpiceDB unconfigured")
		_, isAgentUI := deps.(agentui.Deps)
		assert.False(t, isAgentUI, "/agent-ui must be absent (agentui.Deps cast fails) when SpiceDB unconfigured (no CheckInteract)")
		_, isSessions := deps.(sessions.Deps)
		assert.False(t, isSessions, "/sessions must be absent (sessions.Deps cast fails) when SpiceDB unconfigured (no CheckInteract/LookupInteractableSessions)")
	})

	t.Run("fully configured: all six casts succeed", func(t *testing.T) {
		var deps webui.Deps = &artifactViewDeps{webdDeps: &webdDeps{externalBaseURL: func() string { return "https://trusted.example" }}}
		_, isWebDeps := deps.(identityd.WebDeps)
		assert.True(t, isWebDeps, "viewer-capable umbrella must also host identityd (WebDeps cast succeeds)")
		_, isViewer := deps.(artifactview.Deps)
		assert.True(t, isViewer, "viewer routes must mount (artifactview.Deps cast succeeds)")
		_, isInteract := deps.(interact.Deps)
		assert.True(t, isInteract, "/interact routes must mount (interact.Deps cast succeeds)")
		_, isSessionView := deps.(sessionview.Deps)
		assert.True(t, isSessionView, "/session-view routes must mount (sessionview.Deps cast succeeds)")
		_, isAgentUI := deps.(agentui.Deps)
		assert.True(t, isAgentUI, "/agent-ui routes must mount (agentui.Deps cast succeeds)")
		_, isSessions := deps.(sessions.Deps)
		assert.True(t, isSessions, "/sessions routes must mount (sessions.Deps cast succeeds)")
	})
}

// TestAgentUIRoutes_DegradedWebdDeps_NoCheckInteract_FailsClosedAndLogsLoudly
// is the fix-verification test the task-1 review demanded: it exercises the
// REAL *webdDeps type (not a lookalike fake) in the exact degraded shape
// internal/cmd/webd builds when the artifact-viewer's own prerequisites are
// unconfigured (SpiceDB/memory-token/operator-URL absent) — K8s present,
// Logger present (as of this fix), CheckInteract genuinely absent — and
// proves agentui.Routes both (a) stays unrouted, because CheckInteract really
// is missing and the plugin must not serve without its one authorization
// gate, and (b) logs the failure through *webdDeps's now-present Logger()
// rather than staying silent. Before this fix, *webdDeps had no Logger()
// either, so this exact case fell through to a silent 404 — see
// buildArtifactViewDeps's nil-on-unconfigured-prerequisites return path and
// the webdDeps type's own doc comment in main.go.
func TestAgentUIRoutes_DegradedWebdDeps_NoCheckInteract_FailsClosedAndLogsLoudly(t *testing.T) {
	var mu sync.Mutex
	var logged []string
	capLogger := funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, args)
	}, funcr.Options{})

	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	k8s := fake.NewClientBuilder().WithScheme(scheme).Build()

	// The bare identity-only umbrella: has K8s and (after this fix) Logger,
	// but no CheckInteract — identityd never needed one.
	d := &webdDeps{k8s: k8s, logger: capLogger}

	routes := agentui.New().Routes(d)

	assert.Nil(t, routes, "no CheckInteract means /agent-ui must stay unrouted even though K8s+Logger are present")

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, logged, "the degraded *webdDeps case (K8s+Logger present, CheckInteract absent) must be logged, not silent")
	joined := strings.Join(logged, "\n")
	assert.Contains(t, joined, "agentui", "log must identify the failing plugin")
	assert.Contains(t, joined, "cast", "log must identify this as a deps-cast failure")
}

// TestSessionsRoutes_DegradedWebdDeps_NoCheckInteract_FailsClosedAndLogsLoudly
// is TestAgentUIRoutes_DegradedWebdDeps_NoCheckInteract_FailsClosedAndLogsLoudly's
// sibling for pkg/web/webui/sessions: it exercises the REAL *webdDeps type (not
// sessions_test.go's local fakes) in the exact degraded shape internal/cmd/webd builds
// when the artifact-viewer's own prerequisites are unconfigured — K8s and
// Logger present, CheckInteract and LookupInteractableSessions genuinely
// absent — and proves sessions.Routes both (a) stays unrouted and (b) logs
// the failure through *webdDeps's Logger() rather than staying silent. Uses
// the bare *webdDeps type directly (not wrapped in *artifactViewDeps), so
// there is no risk of setting the logger on a field that *artifactViewDeps's
// own Logger() override would shadow — see that method's doc comment for why
// a fixture built the other way could pass vacuously.
func TestSessionsRoutes_DegradedWebdDeps_NoCheckInteract_FailsClosedAndLogsLoudly(t *testing.T) {
	var mu sync.Mutex
	var logged []string
	capLogger := funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, args)
	}, funcr.Options{})

	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	k8s := fake.NewClientBuilder().WithScheme(scheme).Build()

	// The bare identity-only umbrella: has K8s and Logger, but no
	// CheckInteract/LookupInteractableSessions — identityd never needed either.
	d := &webdDeps{k8s: k8s, logger: capLogger}

	routes := sessions.New().Routes(d)

	assert.Nil(t, routes, "no CheckInteract/LookupInteractableSessions means /sessions must stay unrouted even though K8s+Logger are present")

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, logged, "the degraded *webdDeps case (K8s+Logger present, CheckInteract/LookupInteractableSessions absent) must be logged, not silent")
	joined := strings.Join(logged, "\n")
	assert.Contains(t, joined, "sessions", "log must identify the failing plugin")
	assert.Contains(t, joined, "cast", "log must identify this as a deps-cast failure")
}

// TestSessionViewRoute_MountedAndRequiresAuthCookie is the end-to-end webd
// wiring smoke test: it builds the REAL webui.Server exactly as run() does —
// registry.All() (populated by every blank-imported WebUI, including
// sessionview once main.go blank-imports it) mounted behind the umbrella deps
// — and proves GET /session-view/{ns}/{name} is (a) actually registered
// (never a bare 404) and (b) gated behind AuthAuthenticated: a cookie-less
// request must get 401, never reach the page handler's own CheckInteract
// logic (that logic is sessionview's own package tests' job — see
// pkg/web/webui/sessionview/sessionview_test.go).
func TestSessionViewRoute_MountedAndRequiresAuthCookie(t *testing.T) {
	// identityd is ALSO blank-imported by main.go (and so is present in
	// registry.All() here) and its Routes() panics at server-build time
	// unless K8s/LinkSigner/ExternalBaseURL are all set — build a full
	// webdDeps, not the bare externalBaseURL-only stub other cast tests use.
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	k8s := fake.NewClientBuilder().WithScheme(scheme).Build()
	signer := passthroughlink.New(make([]byte, 32),
		passthroughlink.WithIssuer(passthroughlink.IssuerIdentityd),
		passthroughlink.WithAudience(passthroughlink.AudienceIdentityd))

	authenticate := func(r *http.Request) (string, bool) { return "", false } // no cookie, ever
	deps := &artifactViewDeps{webdDeps: &webdDeps{
		k8s:             k8s,
		linkSigner:      signer,
		externalBaseURL: func() string { return "https://trusted.example" },
	}}

	s, err := webui.NewServer(authenticate, nil,
		func() string { return "trusted.example" }, func() string { return "sandbox.example" },
		nil, deps, registry.All())
	require.NoError(t, err, "webd's real registry.All() must build a valid server")

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "http://trusted.example/session-view/ns1/sess1", nil)
	r.Host = "trusted.example"
	s.ServeHTTP(rec, r)

	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"the session-view route must be mounted (not 404) AND require auth (not e.g. 200/403 from a missing gate)")
}

// TestAgentUIAddress_MountedAndRedirectsIntoTheShell is
// TestSessionViewRoute_MountedAndRequiresAuthCookie's sibling for
// GET /agent-ui/{ns}/{name}: it builds the REAL webui.Server exactly as run()
// does — registry.All() mounted behind the viewer-capable umbrella, which
// main.go's own `var _ agentui.Deps = (*artifactViewDeps)(nil)` guard proves
// satisfies agentui.Deps — and proves the address is actually registered
// (never a bare 404).
//
// It answers a redirect rather than a 401 because it resolves nothing: it is
// mounted AuthNone precisely so an address handed out in a Slack button or
// pasted to a colleague lands its holder in the shell's own login flow with a
// `next` that returns them here, rather than dead-ending on a 401. The
// authorization is the target's, and the shell runs its own fully-consistent
// CheckInteract on the selection before resolving anything about it.
//
// Earlier draft of this test also carried a doc comment (and a companion
// AST-parsing test, since removed) explaining that a registry.All()-based
// check couldn't prove main.go actually imports pkg/web/webui/agentui, because
// this file's own compile-time guard imported it too and Go runs an
// imported package's init() regardless of which file does the importing.
// That gap is now closed differently: the guard itself moved into main.go
// (next to the artifactViewDeps struct it certifies), where it references
// agentui.Deps directly — so the import is load-bearing there, and deleting
// it fails `go build ./internal/cmd/webd` outright rather than only a runtime cast or
// a source-parsing test. See main.go's copy of the guard for that reasoning.
func TestAgentUIAddress_MountedAndRedirectsIntoTheShell(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	k8s := fake.NewClientBuilder().WithScheme(scheme).Build()
	signer := passthroughlink.New(make([]byte, 32),
		passthroughlink.WithIssuer(passthroughlink.IssuerIdentityd),
		passthroughlink.WithAudience(passthroughlink.AudienceIdentityd))

	authenticate := func(r *http.Request) (string, bool) { return "", false } // no cookie, ever
	deps := &artifactViewDeps{webdDeps: &webdDeps{
		k8s:             k8s,
		linkSigner:      signer,
		externalBaseURL: func() string { return "https://trusted.example" },
	}}

	s, err := webui.NewServer(authenticate, nil,
		func() string { return "trusted.example" }, func() string { return "sandbox.example" },
		nil, deps, registry.All())
	require.NoError(t, err, "webd's real registry.All() must build a valid server")

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "http://trusted.example/agent-ui/ns1/sess1", nil)
	r.Host = "trusted.example"
	s.ServeHTTP(rec, r)

	require.Equal(t, http.StatusFound, rec.Code,
		"the agent-ui address must be mounted (not 404) and answer a redirect, not a 401")
	assert.Equal(t, "/sessions?session=ns1%2Fsess1", rec.Header().Get("Location"),
		"it must carry the addressed session into the shell's own selection")
}

// TestSessionsRoute_MountedAndRequiresAuthCookie is
// TestAgentUIAddress_MountedAndRedirectsIntoTheShell's sibling for GET /sessions:
// it builds the REAL webui.Server exactly as run() does — registry.All()
// mounted behind the viewer-capable umbrella, which main.go's own
// `var _ sessions.Deps = (*artifactViewDeps)(nil)` guard proves satisfies
// sessions.Deps — and proves the route is (a) actually registered (never a
// bare 404) and (b) gated: a cookie-less request must get 401, never reach
// shellPageBuild's own logic (that logic is pkg/web/webui/sessions' own package
// tests' job — see sessions_test.go).
//
// /sessions is AuthLoginIfNecessary, not AuthAuthenticated like /agent-ui —
// but with beginLogin nil (as passed here, matching every sibling in this
// file), webui.Server.wrap's AuthLoginIfNecessary branch falls through to the
// same 401 system page an AuthAuthenticated route's failed authenticate does
// (see server.go's wrap: `s.beginLogin != nil` guards the redirect). A
// cookie-less request therefore still 401s here, not 302s — this test does
// NOT exercise the session-less-login-link redirect path; that is
// needsSessionlessLogin's own table-driven coverage (TestNeedsSessionlessLogin).
func TestSessionsRoute_MountedAndRequiresAuthCookie(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	k8s := fake.NewClientBuilder().WithScheme(scheme).Build()
	signer := passthroughlink.New(make([]byte, 32),
		passthroughlink.WithIssuer(passthroughlink.IssuerIdentityd),
		passthroughlink.WithAudience(passthroughlink.AudienceIdentityd))

	authenticate := func(r *http.Request) (string, bool) { return "", false } // no cookie, ever
	deps := &artifactViewDeps{webdDeps: &webdDeps{
		k8s:             k8s,
		linkSigner:      signer,
		externalBaseURL: func() string { return "https://trusted.example" },
	}}

	s, err := webui.NewServer(authenticate, nil,
		func() string { return "trusted.example" }, func() string { return "sandbox.example" },
		nil, deps, registry.All())
	require.NoError(t, err, "webd's real registry.All() must build a valid server")

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "http://trusted.example/sessions", nil)
	r.Host = "trusted.example"
	s.ServeHTTP(rec, r)

	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"the sessions route must be mounted (not 404) AND require auth (not e.g. 200/403 from a missing gate)")
}

// TestStartBrowserSession_GatedOnEveryCollaborator pins
// (*artifactViewDeps).StartBrowserSession's guard: a start function is handed
// out only when this process can actually carry a browser session's outbound
// traffic. That needs NATS, SpiceDB (Authz) and the operator URL — the three
// pkg/web/webui/chat's own ensureRegistry checks before it builds the
// browser.NewHost relay, asked here through chat.CanHostBrowserSessions rather
// than re-listed — plus a real *spicedb.Client for the authz.Granter that
// writes the new session's started_by.
//
// A narrower guard would let both start routes mount, create a real
// AgentSession, and answer CheckInteract=true and GET=Attached while every
// reply the runner sends is silently dropped — nothing at creation time,
// nothing in the response, nothing to diagnose from. Each row zeroes exactly
// ONE collaborator, so a guard that dropped any single check would still pass
// every row but that one.
func TestStartBrowserSession_GatedOnEveryCollaborator(t *testing.T) {
	spdb := &spicedb.Client{} // never dialed; only its non-nilness is under test
	nc := &natsgo.Conn{}      // never connected; only its non-nilness is under test

	cases := []struct {
		name        string
		spdb        *spicedb.Client
		nc          *natsgo.Conn
		operatorURL string
		wantNonNil  bool
	}{
		{name: "every collaborator present: non-nil", spdb: spdb, nc: nc, operatorURL: "http://operator:8082", wantNonNil: true},
		{name: "spdb nil (SpiceDB unconfigured): nil", spdb: nil, nc: nc, operatorURL: "http://operator:8082"},
		{name: "NATS unconfigured: nil, not a silent partial mount", spdb: spdb, nc: nil, operatorURL: "http://operator:8082"},
		{name: "operator URL unconfigured: nil", spdb: spdb, nc: nc, operatorURL: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &artifactViewDeps{
				webdDeps:    &webdDeps{logger: logr.Discard()},
				spdb:        tc.spdb,
				nc:          tc.nc,
				operatorURL: tc.operatorURL,
			}
			got := d.StartBrowserSession()
			if tc.wantNonNil {
				assert.NotNil(t, got, "every prerequisite present: StartBrowserSession must be non-nil")
			} else {
				assert.Nil(t, got, "a missing prerequisite must make StartBrowserSession nil, never a partially-working start")
			}
		})
	}
}

// TestStartRoute_OmittedAndLoggedWhenNATSUnconfigured is
// TestAgentUIAddress_MountedAndRedirectsIntoTheShell's sibling for the combination
// that is easiest to get wrong: SpiceDB wired, but NATS unconfigured, so the
// browser Channel's relay cannot exist. The start route must be OMITTED, and
// pkg/web/webui/agentui's Routes must log why — a nil collaborator that neither
// mounts nor logs is a silent 404 with nothing in the logs, the exact class
// of bug this feature's own Deps-cast-failure guard exists to prevent.
func TestStartRoute_OmittedAndLoggedWhenNATSUnconfigured(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	k8s := fake.NewClientBuilder().WithScheme(scheme).Build()
	signer := passthroughlink.New(make([]byte, 32),
		passthroughlink.WithIssuer(passthroughlink.IssuerIdentityd),
		passthroughlink.WithAudience(passthroughlink.AudienceIdentityd))

	var mu sync.Mutex
	var logged []string
	capLogger := funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, args)
	}, funcr.Options{})

	authenticate := func(r *http.Request) (string, bool) { return "", false } // no cookie, ever
	deps := &artifactViewDeps{
		webdDeps: &webdDeps{
			k8s:             k8s,
			linkSigner:      signer,
			externalBaseURL: func() string { return "https://trusted.example" },
		},
		spdb:        &spicedb.Client{}, // SpiceDB is wired...
		nc:          nil,               // ...but NATS is NOT: chat's own relay cannot exist.
		operatorURL: "http://operator:8082",
		// logger is artifactViewDeps' OWN field — Logger() (below) returns
		// THIS one, not the embedded *webdDeps.logger, so the capture must be
		// wired here for agentui.Routes' log line to reach it.
		logger: capLogger,
	}

	s, err := webui.NewServer(authenticate, nil,
		func() string { return "trusted.example" }, func() string { return "sandbox.example" },
		nil, deps, registry.All())
	require.NoError(t, err, "webd's real registry.All() must build a valid server")

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "http://trusted.example/agent-ui/ns1/sess1/start", nil)
	r.Host = "trusted.example"
	s.ServeHTTP(rec, r)

	assert.Equal(t, http.StatusNotFound, rec.Code,
		"the start route must be OMITTED (404), not mounted, when NATS is unconfigured even though SpiceDB is wired")

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(logged, "\n")
	assert.Contains(t, joined, "start", "the omission must be LOGGED, not silently dropped")
}

// TestManifestResolutionForRealPlugins is internal/cmd/webd's answer to
// pkg/web/webui/manifest_contract_test.go's TestRegisteredPageAppsResolveInManifest,
// which passes VACUOUSLY for every page gated behind a non-trivial Deps cast
// (session-view, artifact-view — interact and agent-ui carry no Page at all,
// so it was never covered by that test either way). That test's blind spot
// is two-fold: it runs inside pkg/web/webui's own test binary, where
// registry.All() contains none of the real plugins (nobody there
// blank-imports them), AND it calls ui.Routes(nil), so even a plugin that
// somehow WAS registered would fail its Deps cast and contribute zero routes
// to the assertion loop — a Page.App that references an unbuilt/renamed
// React entry could ship undetected.
//
// Both gaps are closed here: this is internal/cmd/webd's own test binary, so
// registry.All() is genuinely populated by main.go's real blank/named
// imports (the same registration the production webd binary gets), and
// `deps` is the same fully-capable *artifactViewDeps
// TestAgentUIAddress_MountedAndRedirectsIntoTheShell /
// TestSessionViewRoute_MountedAndRequiresAuthCookie use — TestUmbrellaDepsCasts
// above already proves it satisfies artifactview.Deps, interact.Deps,
// sessionview.Deps, and agentui.Deps simultaneously, so Routes(deps) returns
// each plugin's REAL routes, not the nil-cast empty list.
func TestManifestResolutionForRealPlugins(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	k8s := fake.NewClientBuilder().WithScheme(scheme).Build()
	signer := passthroughlink.New(make([]byte, 32),
		passthroughlink.WithIssuer(passthroughlink.IssuerIdentityd),
		passthroughlink.WithAudience(passthroughlink.AudienceIdentityd))
	deps := &artifactViewDeps{webdDeps: &webdDeps{
		k8s:             k8s,
		linkSigner:      signer,
		externalBaseURL: func() string { return "https://trusted.example" },
	}}

	mf, err := webassets.Manifest()
	require.NoError(t, err, "embedded manifest must parse")

	// wantPageApps tracks the page apps gated behind a Deps cast that
	// pkg/web/webui's version of this test can never reach. Recording whether
	// each was actually SEEN (not just "did the loop run at all") means a
	// future change that makes `deps` stop satisfying one of these plugins'
	// Deps interfaces fails HERE, naming the app, instead of silently
	// narrowing this test back toward the vacuous shape it replaces.
	//
	// "agent-ui" is deliberately absent: that plugin serves no page at all
	// now — its address redirects into the shell, and the shell's own
	// "sessions" app is what renders the agent-defined view. agentSeen below
	// keeps this test's Deps-cast coverage for it, which is the half a page
	// app was standing in for.
	wantPageApps := map[string]bool{"session-view": false, "artifact-view": false, "sessions": false}
	interactSeen := false
	agentUISeen := false

	for _, ui := range registry.All() {
		routes := ui.Routes(deps)
		switch ui.Name() {
		case "interact":
			// interact is Handler-only (no Page, so no manifest entry to check) —
			// its coverage here is just "the Deps cast succeeded and it produced
			// routes", proven separately from the Page/manifest loop below.
			interactSeen = len(routes) > 0
		case "agent-ui":
			// Same shape, for the same reason: no Page to resolve, but a Deps
			// cast that must keep succeeding or all four of its routes vanish.
			agentUISeen = len(routes) > 0
		}
		for _, rt := range routes {
			if rt.Page == nil {
				continue
			}
			if _, tracked := wantPageApps[rt.Page.App]; tracked {
				wantPageApps[rt.Page.App] = true
			}
			entry, ok := mf[rt.Page.App]
			assert.Truef(t, ok, "WebUI %q route %q references appKey %q absent from pkg/web/webui/webassets/dist/manifest.json — run `mage web:build`",
				ui.Name(), rt.Pattern, rt.Page.App)
			assert.NotEmptyf(t, entry.Scripts, "WebUI %q route %q's manifest entry %q has no script — run `mage web:build`",
				ui.Name(), rt.Pattern, rt.Page.App)
		}
	}

	for app, seen := range wantPageApps {
		assert.Truef(t, seen, "page app %q was never exercised by this test — its plugin's Deps cast must have failed against the fixture deps, which would make this test vacuous for %q", app, app)
	}
	assert.True(t, interactSeen, `"interact" was never exercised by this test — its Deps cast must have failed against the fixture deps`)
	assert.True(t, agentUISeen, `"agent-ui" was never exercised by this test — its Deps cast must have failed against the fixture deps`)
}

// TestDerivePause covers the AgentSession phase → (paused, cause) mapping that
// seeds the live-view status, including Succeeded → done (PauseCauseComplete) and
// the pending-approval overrides.
func TestDerivePause(t *testing.T) {
	withPhase := func(p string) *spiceboxv1alpha1.AgentSession {
		s := &spiceboxv1alpha1.AgentSession{}
		s.Status.Phase = p
		return s
	}
	cases := []struct {
		name       string
		sess       *spiceboxv1alpha1.AgentSession
		wantPaused bool
		wantCause  string
	}{
		{"running → working", withPhase(spiceboxv1alpha1.AgentSessionPhaseRunning), false, ""},
		{"pending → working", withPhase(spiceboxv1alpha1.AgentSessionPhasePending), false, ""},
		{"idle → awaiting reply", withPhase(spiceboxv1alpha1.AgentSessionPhaseIdle), true, channelevents.PauseCauseReply},
		{"succeeded → complete (done)", withPhase(spiceboxv1alpha1.AgentSessionPhaseSucceeded), true, channelevents.PauseCauseComplete},
		{"failed → failed", withPhase(spiceboxv1alpha1.AgentSessionPhaseFailed), true, channelevents.PauseCauseFailed},
		{"awaiting retry → retry", withPhase(spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry), true, channelevents.PauseCauseRetry},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, c := derivePause(tc.sess)
			assert.Equal(t, tc.wantPaused, p)
			assert.Equal(t, tc.wantCause, c)
		})
	}

	t.Run("pending tool_approval interaction overrides an active phase → approval", func(t *testing.T) {
		s := withPhase(spiceboxv1alpha1.AgentSessionPhaseRunning)
		s.Status.PendingInteractions = []spiceboxv1alpha1.PendingInteraction{
			{RequestID: "tg1", Category: categories.ToolApproval},
		}
		p, c := derivePause(s)
		assert.True(t, p)
		assert.Equal(t, channelevents.PauseCauseApproval, c)
	})

	t.Run("pending content_inspection interaction (generic list) overrides an active phase → approval", func(t *testing.T) {
		s := withPhase(spiceboxv1alpha1.AgentSessionPhaseRunning)
		s.Status.PendingInteractions = []spiceboxv1alpha1.PendingInteraction{
			{RequestID: "ci1", Category: categories.ContentInspection},
		}
		p, c := derivePause(s)
		assert.True(t, p)
		assert.Equal(t, channelevents.PauseCauseApproval, c)
	})

	t.Run("pending info_leakage interaction (generic list) → leakage-approval pause (checked first)", func(t *testing.T) {
		s := withPhase(spiceboxv1alpha1.AgentSessionPhaseRunning)
		s.Status.PendingInteractions = []spiceboxv1alpha1.PendingInteraction{
			{RequestID: "ci1", Category: categories.ContentInspection},
			{RequestID: "lk1", Category: categories.InfoLeakage},
		}
		p, c := derivePause(s)
		assert.True(t, p)
		assert.Equal(t, channelevents.PauseCauseLeakageApproval, c)
	})
}

// TestNeedsSessionlessLogin covers which cookie-less AuthLoginIfNecessary routes
// get a session-less login link minted on the fly (vs. a hard 401): the /admin
// console, the platform-admin artifact viewer opened with unsigned artifactId
// params, and the /sessions dashboard. A signed artifact link (d+sig) never
// reaches this decision, and a bare /artifact-view with no artifactId must NOT
// mint one. /chat and /chat/ws are asserted as ordinary no-mint paths:
// pkg/web/webui/chat's own Page route is gone, so nothing routes there anymore —
// this guards against silently reviving the dead special-case without a real
// AuthLoginIfNecessary Page behind it. /sessions/api/sessions is asserted as a
// no-mint path too: pkg/web/webui/sessions' API routes are AuthAuthenticated, not
// AuthLoginIfNecessary, so a cookie-less request there must hard-401, never
// silently redirect through a minted login link meant for the page.
func TestNeedsSessionlessLogin(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		artifactID string
		want       bool
	}{
		{"/admin console → mint", "/admin", "", true},
		{"/admin subpath → mint", "/admin/sessions", "", true},
		{"/sessions dashboard → mint", "/sessions", "", true},
		{"/sessions/api/sessions (AuthAuthenticated, not this page) → no mint", "/sessions/api/sessions", "", false},
		{"/chat (no route serves it anymore) → no mint", "/chat", "", false},
		{"/chat/ws (no route serves it anymore) → no mint", "/chat/ws", "", false},
		{"unsigned artifact viewer (artifactId set) → mint", "/artifact-view", "art-1", true},
		{"bare /artifact-view (no artifactId) → no mint", "/artifact-view", "", false},
		{"unrelated route → no mint", "/oidc/login", "", false},
		{"artifactId on a non-viewer path → no mint", "/something", "art-1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, needsSessionlessLogin(tc.path, tc.artifactID))
		})
	}
}

// TestWatchSessionStatus_TurnActivityClearsWorking is the regression test for the
// stuck-"working" bug: on agent_work_complete the runner publishes a planless
// KindTurnActivity{Active:false} (no plan_update fires when no plan step is
// in_progress), so WatchSessionStatus MUST subscribe to it and flip Paused — else
// a viewer connected through completion never clears the "working" animation.
func TestWatchSessionStatus_TurnActivityClearsWorking(t *testing.T) {
	srv := natstest.RunServer(&natsserver.Options{Port: -1})
	defer srv.Shutdown()
	subConn, err := natsgo.Connect(srv.ClientURL())
	require.NoError(t, err)
	defer subConn.Close()
	pubConn, err := natsgo.Connect(srv.ClientURL())
	require.NoError(t, err)
	defer pubConn.Close()

	const ns, sess = "default", "s1"
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	as := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: sess}}
	as.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseRunning
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(as).Build()

	d := &artifactViewDeps{webdDeps: &webdDeps{k8s: k}, nc: subConn, logger: logr.Discard()}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := d.WatchSessionStatus(ctx, ns, sess)
	require.NoError(t, err)
	require.NotNil(t, ch)
	require.NoError(t, subConn.Flush()) // subscriptions registered server-side before we publish

	select {
	case seed := <-ch:
		assert.False(t, seed.Paused, "a Running session seeds as working")
	case <-time.After(2 * time.Second):
		t.Fatal("no seed status emitted")
	}

	// agent_work_complete → idle publishes only the planless turn_activity.
	require.NoError(t, channelevents.PublishOut(
		func(subj string, data []byte) error { return pubConn.Publish(subj, data) },
		ns, sess, channelevents.KindTurnActivity,
		channelevents.TurnActivityPayload{Active: false, Cause: channelevents.PauseCauseReply},
	))
	require.NoError(t, pubConn.Flush())

	select {
	case got := <-ch:
		assert.True(t, got.Paused, "turn_activity Active=false must clear 'working' (Paused=true)")
		assert.Equal(t, channelevents.PauseCauseReply, got.PauseCause)
	case <-time.After(2 * time.Second):
		t.Fatal("live-view never received the completion (turn_activity) update")
	}
}

// TestUserMessageToMirror_UserEchoToMirror covers the pure payload→
// MirrorMessage mapping WatchMessages uses on each NATS callback — the seam
// that's testable without a live NATS connection.
func TestUserMessageToMirror_UserEchoToMirror(t *testing.T) {
	t.Run("respond_to_user reply maps to an agent-role mirror line", func(t *testing.T) {
		got := userMessageToMirror(channelevents.OutboundUserMessagePayload{Text: "hello there"})
		assert.Equal(t, artifactview.MirrorMessage{Role: "agent", Text: "hello there"}, got)
	})

	t.Run("user_echo maps to a user-role mirror line with author + via", func(t *testing.T) {
		got := userEchoToMirror(channelevents.UserEchoPayload{
			Text:   "what's the status?",
			Author: channelevents.ExternalIdentity{Email: "user@example.com"},
			Via:    "the artifact view",
		})
		assert.Equal(t, artifactview.MirrorMessage{
			Role:   "user",
			Text:   "what's the status?",
			Author: "user@example.com",
			Via:    "the artifact view",
		}, got)
	})

	t.Run("user_echo with no author email maps to an empty display author", func(t *testing.T) {
		got := userEchoToMirror(channelevents.UserEchoPayload{Text: "hi"})
		assert.Equal(t, "", got.Author)
	})
}

// TestWatchMessages_NilNATS_DegradesToNoChatStream mirrors WatchSessionStatus's
// degrade-when-unwired contract: with no NATS client, the live-view must get
// (nil, nil) — a chat-stream-disabled signal, not an error.
func TestWatchMessages_NilNATS_DegradesToNoChatStream(t *testing.T) {
	d := &artifactViewDeps{webdDeps: &webdDeps{}, logger: logr.Discard()}
	ch, err := d.WatchMessages(context.Background(), "default", "sess1")
	require.NoError(t, err)
	assert.Nil(t, ch)
}

// TestWatchMessages_UserMessageAndUserEcho is the behavioral regression test:
// an agent respond_to_user reply and a user_echo (view-originated send
// mirrored back by channelsd) must each surface as a MirrorMessage on the
// channel WatchMessages returns, with Role/Text/Author/Via populated from the
// envelope payload and At stamped at receive time.
func TestWatchMessages_UserMessageAndUserEcho(t *testing.T) {
	srv := natstest.RunServer(&natsserver.Options{Port: -1})
	defer srv.Shutdown()
	subConn, err := natsgo.Connect(srv.ClientURL())
	require.NoError(t, err)
	defer subConn.Close()
	pubConn, err := natsgo.Connect(srv.ClientURL())
	require.NoError(t, err)
	defer pubConn.Close()

	const ns, sess = "default", "s1"
	d := &artifactViewDeps{webdDeps: &webdDeps{}, nc: subConn, logger: logr.Discard()}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := d.WatchMessages(ctx, ns, sess)
	require.NoError(t, err)
	require.NotNil(t, ch)
	require.NoError(t, subConn.Flush()) // subscriptions registered server-side before we publish

	publish := func(subj string, data []byte) error { return pubConn.Publish(subj, data) }

	require.NoError(t, channelevents.PublishOut(publish, ns, sess, channelevents.KindUserMessage,
		channelevents.OutboundUserMessagePayload{Text: "here is your answer"}))
	require.NoError(t, pubConn.Flush())

	select {
	case got := <-ch:
		assert.Equal(t, "agent", got.Role)
		assert.Equal(t, "here is your answer", got.Text)
		_, perr := time.Parse(time.RFC3339, got.At)
		assert.NoError(t, perr, "At must be a stamped RFC3339 timestamp")
	case <-time.After(2 * time.Second):
		t.Fatal("no MirrorMessage emitted for respond_to_user (user_message)")
	}

	require.NoError(t, channelevents.PublishOut(publish, ns, sess, channelevents.KindUserEcho,
		channelevents.UserEchoPayload{
			Text:   "what's the status?",
			Author: channelevents.ExternalIdentity{Email: "user@example.com"},
			Via:    "the artifact view",
		}))
	require.NoError(t, pubConn.Flush())

	select {
	case got := <-ch:
		assert.Equal(t, "user", got.Role)
		assert.Equal(t, "what's the status?", got.Text)
		assert.Equal(t, "user@example.com", got.Author)
		assert.Equal(t, "the artifact view", got.Via)
	case <-time.After(2 * time.Second):
		t.Fatal("no MirrorMessage emitted for user_echo")
	}
}

// Every Page-bearing route webd actually mounts must name a React entry that
// exists in the committed build manifest. A Go Route.App referencing an unbuilt
// or renamed Vite entry compiles and serves a shell that loads nothing, and the
// only symptom is a blank page on first load.
//
// This assertion lives HERE rather than in pkg/web/webui because it needs real Deps.
// pkg/web/webui's own contract test can only reach registry.All() with nil Deps, and
// every Page-bearing UI fails closed on nil Deps and returns no routes at all —
// so blank-importing them there would still assert nothing. webd is where real
// deps exist, and webd is the binary whose blank imports decide the actual set.
//
// Reaches three of the four Page-bearing UIs: admin, artifact-live-view and
// session-view. The fourth, chat, gates its Routes on a started outbound relay
// over a live NATS connection AND installs a process-wide registry singleton, so
// mounting it here would both cost an embedded server and leak into sibling
// tests; it stays covered by pkg/web/webui/chat's own tests. health and identity
// declare no Page routes at all, so there is nothing of theirs to resolve.
func TestRegisteredPageAppsResolveInBuiltManifest(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	k8s := fake.NewClientBuilder().WithScheme(scheme).Build()
	signer := passthroughlink.New(make([]byte, 32),
		passthroughlink.WithIssuer(passthroughlink.IssuerIdentityd),
		passthroughlink.WithAudience(passthroughlink.AudienceIdentityd))
	// admindURL/admindToken are set so adminui.Routes does NOT fail closed —
	// without them the admin app key would go unchecked.
	deps := &artifactViewDeps{webdDeps: &webdDeps{
		k8s: k8s, linkSigner: signer,
		externalBaseURL: func() string { return "https://trusted.example" },
	}, admindURL: "http://admind.example", admindToken: "tok"}

	mf, err := webassets.Manifest()
	require.NoError(t, err, "embedded manifest must parse")

	seen := map[string]string{} // appKey -> the route that named it
	for _, ui := range registry.All() {
		for _, rt := range ui.Routes(deps) {
			if rt.Page == nil {
				continue
			}
			seen[rt.Page.App] = ui.Name() + " " + rt.Pattern
			assert.Containsf(t, mf, rt.Page.App,
				"WebUI %q route %q references appKey %q absent from pkg/web/webui/webassets/dist/manifest.json — run `mage web:build`",
				ui.Name(), rt.Pattern, rt.Page.App)
		}
	}
	// Without this the test would pass silently if a future deps change made every
	// UI fail closed — exactly how the pkg/web/webui version of it became vacuous.
	require.NotEmpty(t, seen, "no Page route resolved at all; the deps fixture above no longer satisfies any UI")
	t.Logf("checked %d page apps: %v", len(seen), seen)
}
