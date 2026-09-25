package interact

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/artifactview"
)

const testTrustedOrigin = "https://trusted.example"

// callTracker records whether a given Deps method was invoked during a
// request — used by the session-scoped tests to prove CheckArtifactView is
// NEVER called on that path (it is a strict superset permission that must
// gate the artifact path only).
type callTracker struct {
	artifactViewCalled  bool
	listRevisionsCalled bool
}

// fakeDeps is a hand-rolled Deps for testing the gate matrix: each field
// controls exactly one gate's outcome, independent of the others.
type fakeDeps struct {
	// noSubject, when true, means postInteract does NOT inject an
	// authenticated subject into the request context (simulates an
	// unauthenticated caller).
	noSubject bool

	// interact is CheckInteract's ok return; interactErr forces it to error
	// instead (simulating a SpiceDB outage — must fail closed as 503).
	interact    bool
	interactErr bool

	// class is the AgentClass AgentClassOf resolves; classErr forces
	// AgentClassOf to error instead.
	class    *spiceboxv1alpha1.AgentClass
	classErr bool

	// artifactOK is CheckArtifactView's ok return; artifactErr forces it to
	// error instead.
	artifactOK  bool
	artifactErr bool

	// artifactNotInSession, when true, makes the ListRevisions binding probe
	// error the way a scoped artifact read does for an id that does not live
	// in the requested session — the "right permission, wrong session" case
	// CheckArtifactView alone cannot detect.
	artifactNotInSession bool

	// nats, when set, replaces the default NATSRequest stub (used by the two
	// Via-minting tests to inspect the envelope actually sent over the wire).
	nats channelevents.RequestFunc

	// calls, when set, records which Deps methods were invoked — used by the
	// session-scoped tests to assert CheckArtifactView is never reached.
	calls *callTracker
}

func (d fakeDeps) CheckInteract(_ context.Context, _, _, _ string) (bool, error) {
	if d.interactErr {
		return false, errors.New("spicedb: simulated outage")
	}
	return d.interact, nil
}

func (d fakeDeps) CheckArtifactView(_ context.Context, _, _ string) (bool, error) {
	if d.calls != nil {
		d.calls.artifactViewCalled = true
	}
	if d.artifactErr {
		return false, errors.New("spicedb: simulated outage")
	}
	return d.artifactOK, nil
}

// ListRevisions stands in for the scoped artifact read that proves the
// artifact lives in THIS session. Production reads it inside
// memory.Scope{Kind:"session", ID: ns+"/"+sess} and hard-errors
// `artifacts: artifact %q not found` for an id outside that scope; the fake
// reproduces exactly that shape.
func (d fakeDeps) ListRevisions(_ context.Context, _, _, artifactID string) ([]artifactview.RevisionMeta, error) {
	if d.calls != nil {
		d.calls.listRevisionsCalled = true
	}
	if d.artifactNotInSession {
		return nil, errors.New("artifacts: artifact " + artifactID + " not found")
	}
	return []artifactview.RevisionMeta{{Seq: 1, RevisionID: "rev-1"}}, nil
}

func (d fakeDeps) AgentClassOf(_ context.Context, _, _ string) (*spiceboxv1alpha1.AgentClass, error) {
	if d.classErr {
		return nil, errors.New("agentclass: simulated not-found")
	}
	return d.class, nil
}

// WidgetOriginOf is unused by the /interact surface (it sends no artifact-id
// widget reference), so the fake reports "no such widget" rather than
// pretending to have a widget list.
func (d fakeDeps) WidgetOriginOf(_ context.Context, _, _, _ string) (string, bool, error) {
	return "", false, nil
}

func (d fakeDeps) NATSRequest() channelevents.RequestFunc {
	if d.nats != nil {
		return d.nats
	}
	return func(_ string, _ []byte, _ time.Duration) ([]byte, error) {
		return json.Marshal(channelevents.ViewMessageResultPayload{Outcome: "routed"})
	}
}

func (d fakeDeps) TrustedOrigin() string { return testTrustedOrigin }
func (d fakeDeps) Logger() logr.Logger   { return logr.Discard() }

// classWithSessionViews builds an AgentClass whose spec.capabilities grants
// session_views with the given interaction kinds. Called with no kinds, the
// grant body is the empty object {} (granted, but no interactions listed —
// every kind is refused).
func classWithSessionViews(kinds ...string) *spiceboxv1alpha1.AgentClass {
	raw := []byte(`{}`)
	if len(kinds) > 0 {
		b, err := json.Marshal(struct {
			Interactions []string `json:"interactions"`
		}{Interactions: kinds})
		if err != nil {
			panic(err)
		}
		raw = b
	}
	return &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Capabilities: map[string]apiextensionsv1.JSON{
				"session_views": {Raw: raw},
			},
		},
	}
}

// classNoCaps builds an AgentClass with no capabilities map at all —
// session_views is entirely absent (opt-in, so this must fail closed).
func classNoCaps() *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{}
}

// postInteract builds and serves a POST /session/ns1/sess1/interact request
// against interactHandler(d), returning the recorded response. Injects an
// authenticated subject unless d.noSubject is set, and a trusted Origin
// header (steps 1 and 2 are not what most gate-matrix rows are testing).
func postInteract(t *testing.T, d fakeDeps, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/session/ns1/sess1/interact", strings.NewReader(body))
	req.SetPathValue("ns", "ns1")
	req.SetPathValue("name", "sess1")
	req.Header.Set("Origin", testTrustedOrigin)
	if !d.noSubject {
		req = req.WithContext(webui.WithSubjectForTest(req.Context(), "user:tester"))
	}
	rec := httptest.NewRecorder()
	interactHandler(d).ServeHTTP(rec, req)
	return rec
}

func TestInteractGates(t *testing.T) {
	base := func() fakeDeps { // interact=true, session_views granted with user_message, artifactview=true
		return fakeDeps{interact: true, class: classWithSessionViews("user_message"), artifactOK: true}
	}
	cases := []struct {
		name       string
		deps       fakeDeps
		wantStatus int
	}{
		{"all gates pass → 200", base(), 200},
		{"not authenticated → 401", func() fakeDeps { d := base(); d.noSubject = true; return d }(), 401},
		{"interact denied → 403", func() fakeDeps { d := base(); d.interact = false; return d }(), 403},
		{"session_views absent → 403", func() fakeDeps { d := base(); d.class = classNoCaps(); return d }(), 403},
		{"session_views {} but no interactions → 403", func() fakeDeps { d := base(); d.class = classWithSessionViews(); return d }(), 403},
		{"SpiceDB error → 503 (fail closed)", func() fakeDeps { d := base(); d.interactErr = true; return d }(), 503},
		// Named for what it actually exercises: CheckArtifactView is a GLOBAL
		// artifact#view check. It was previously named "artifact not this
		// session", asserting a binding this row never tested and the code did
		// not perform — see TestInteractRefusesArtifactFromAnotherSession.
		{"artifact#view denied → 403", func() fakeDeps { d := base(); d.artifactOK = false; return d }(), 403},
		{"artifact#view held but artifact lives in another session → 403", func() fakeDeps { d := base(); d.artifactNotInSession = true; return d }(), 403},
		{"AgentClassOf errors → 403 (fail closed)", func() fakeDeps { d := base(); d.classErr = true; return d }(), 403},
		{"CheckArtifactView errors → 503 (fail closed)", func() fakeDeps { d := base(); d.artifactErr = true; return d }(), 503},
		{"unknown kind → 400", base(), 400}, // overridden below via a distinct body
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"kind":"user_message","artifactId":"artifact-3f2a1b8c","payload":{"text":"hi"}}`
			if i == len(cases)-1 {
				body = `{"kind":"not_a_real_kind","artifactId":"artifact-3f2a1b8c","payload":{"text":"hi"}}`
			}
			rec := postInteract(t, tc.deps, body)
			assert.Equal(t, tc.wantStatus, rec.Code, "body: %s", rec.Body.String())
		})
	}
}

// TestInteractRefusesArtifactFromAnotherSession is the binding test.
// CheckArtifactView is a GLOBAL artifact#view check — it proves the subject
// may view the artifact, and nothing about WHICH session that artifact lives
// in. A subject holding agentsession#interact on session A and artifact#view
// on artifact X (owned by session B — one they merely participate in) could
// otherwise POST {artifactId: X} to /session/A/interact and have the turn
// durably attributed to an artifact session A does not own, inside the
// server-minted Via the audit log treats as authoritative.
//
// The binding must therefore be RESOLVED server-side, exactly as
// artifactview's bindView does it, and the refusal must land BEFORE the Via
// is minted and submitted.
func TestInteractRefusesArtifactFromAnotherSession(t *testing.T) {
	calls := &callTracker{}
	d := fakeDeps{
		interact:             true,
		class:                classWithSessionViews("user_message"),
		artifactOK:           true, // the subject genuinely holds artifact#view
		artifactNotInSession: true, // but the artifact does not live in ns1/sess1
		calls:                calls,
		nats: func(_ string, _ []byte, _ time.Duration) ([]byte, error) {
			t.Fatal("an unbound artifact must never reach Submit — no Via may be minted for it")
			return nil, nil
		},
	}
	rec := postInteract(t, d, `{"kind":"user_message","artifactId":"artifact-3f2a1b8c","payload":{"text":"hi"}}`)

	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.True(t, calls.listRevisionsCalled, "the artifact→session binding probe must actually run")
	// The refusal is deliberately the same message a plain artifact#view
	// denial gets: distinguishing "wrong session" from "no access" would let a
	// caller enumerate which artifact ids live in which session (same reasoning
	// as artifactview's denyNoAccess).
	assert.NotContains(t, strings.ToLower(rec.Body.String()), "not found",
		"the refusal must not disclose whether the artifact exists elsewhere")
}

func TestInteractMintsArtifactVia(t *testing.T) {
	d := fakeDeps{interact: true, class: classWithSessionViews("user_message"), artifactOK: true,
		nats: func(_ string, data []byte, _ time.Duration) ([]byte, error) {
			var env channelevents.Envelope
			_ = json.Unmarshal(data, &env)
			var pl channelevents.ViewMessagePayload
			_ = json.Unmarshal(env.Payload, &pl)
			assert.Equal(t, "urn:ap:view:artifact:artifact-3f2a1b8c", pl.Via, "server mints the artifact Via")
			return json.Marshal(channelevents.ViewMessageResultPayload{Outcome: "routed"})
		}}
	rec := postInteract(t, d, `{"kind":"user_message","artifactId":"artifact-3f2a1b8c","payload":{"text":"hi"}}`)
	require.Equal(t, 200, rec.Code, "body: %s", rec.Body.String())
}

func TestInteractMintsAnnotationsViaSub(t *testing.T) {
	// annotation_batch's ViaSub() places it at the /annotations sub-URN, so the
	// runner recognizes an annotation turn verifiably from the signed Via.
	d := fakeDeps{interact: true, class: classWithSessionViews("annotation_batch"), artifactOK: true,
		nats: func(_ string, data []byte, _ time.Duration) ([]byte, error) {
			var env channelevents.Envelope
			_ = json.Unmarshal(data, &env)
			var pl channelevents.ViewMessagePayload
			_ = json.Unmarshal(env.Payload, &pl)
			assert.Equal(t, "urn:ap:view:artifact:artifact-3f2a1b8c/annotations", pl.Via,
				"annotation_batch mints the artifact/annotations sub-URN")
			return json.Marshal(channelevents.ViewMessageResultPayload{Outcome: "routed"})
		}}
	rec := postInteract(t, d, `{"kind":"annotation_batch","artifactId":"artifact-3f2a1b8c","payload":{"annotations":[{"comment":"x"}]}}`)
	require.Equal(t, 200, rec.Code, "body: %s", rec.Body.String())
}

func TestInteractIgnoresClientVia(t *testing.T) {
	// A client-supplied "via" in the body must be IGNORED — the server mints it
	// from the (re-checked) artifactId, never from the client.
	d := fakeDeps{interact: true, class: classWithSessionViews("user_message"), artifactOK: true,
		nats: func(_ string, data []byte, _ time.Duration) ([]byte, error) {
			var env channelevents.Envelope
			_ = json.Unmarshal(data, &env)
			var pl channelevents.ViewMessagePayload
			_ = json.Unmarshal(env.Payload, &pl)
			assert.NotContains(t, pl.Via, "evil", "a client-supplied via must never reach the wire")
			return json.Marshal(channelevents.ViewMessageResultPayload{Outcome: "routed"})
		}}
	postInteract(t, d, `{"kind":"user_message","artifactId":"artifact-3f2a1b8c","via":"urn:ap:view:artifact:evil","payload":{"text":"hi"}}`)
}

// TestInteractSessionScoped covers the session-scoped branch (artifactId ==
// ""): no artifact required, gated on CheckInteract ONLY, minting a
// urn:ap:view:session:<ns>/<name> Via.
func TestInteractSessionScoped(t *testing.T) {
	t.Run("granted → 200, mints session Via, never calls CheckArtifactView", func(t *testing.T) {
		calls := &callTracker{}
		d := fakeDeps{
			interact: true,
			class:    classWithSessionViews("user_message"),
			calls:    calls,
			nats: func(_ string, data []byte, _ time.Duration) ([]byte, error) {
				var env channelevents.Envelope
				_ = json.Unmarshal(data, &env)
				var pl channelevents.ViewMessagePayload
				_ = json.Unmarshal(env.Payload, &pl)
				assert.Equal(t, "urn:ap:view:session:ns1/sess1", pl.Via, "server mints the session Via")
				return json.Marshal(channelevents.ViewMessageResultPayload{Outcome: "routed"})
			},
		}
		rec := postInteract(t, d, `{"kind":"user_message","payload":{"text":"hi"}}`)
		require.Equal(t, 200, rec.Code, "body: %s", rec.Body.String())
		assert.False(t, calls.artifactViewCalled, "CheckArtifactView must NEVER be called on the session-scoped path")
	})

	t.Run("session_views absent → 403", func(t *testing.T) {
		calls := &callTracker{}
		d := fakeDeps{interact: true, class: classNoCaps(), calls: calls}
		rec := postInteract(t, d, `{"kind":"user_message","payload":{"text":"hi"}}`)
		assert.Equal(t, 403, rec.Code, "body: %s", rec.Body.String())
		assert.False(t, calls.artifactViewCalled, "CheckArtifactView must NEVER be called on the session-scoped path")
	})

	t.Run("CheckInteract denied → 403", func(t *testing.T) {
		calls := &callTracker{}
		d := fakeDeps{interact: false, class: classWithSessionViews("user_message"), calls: calls}
		rec := postInteract(t, d, `{"kind":"user_message","payload":{"text":"hi"}}`)
		assert.Equal(t, 403, rec.Code, "body: %s", rec.Body.String())
		assert.False(t, calls.artifactViewCalled, "CheckArtifactView must NEVER be called on the session-scoped path")
	})

	t.Run("CheckInteract errors → 503 (fail closed)", func(t *testing.T) {
		calls := &callTracker{}
		d := fakeDeps{interactErr: true, class: classWithSessionViews("user_message"), calls: calls}
		rec := postInteract(t, d, `{"kind":"user_message","payload":{"text":"hi"}}`)
		assert.Equal(t, 503, rec.Code, "body: %s", rec.Body.String())
		assert.False(t, calls.artifactViewCalled, "CheckArtifactView must NEVER be called on the session-scoped path")
	})
}

func TestInteractOriginMismatchIsForbidden(t *testing.T) {
	d := fakeDeps{interact: true, class: classWithSessionViews("user_message"), artifactOK: true}
	req := httptest.NewRequest(http.MethodPost, "/session/ns1/sess1/interact",
		strings.NewReader(`{"kind":"user_message","artifactId":"artifact-3f2a1b8c","payload":{"text":"hi"}}`))
	req.SetPathValue("ns", "ns1")
	req.SetPathValue("name", "sess1")
	req.Header.Set("Origin", "https://evil.example")
	req = req.WithContext(webui.WithSubjectForTest(req.Context(), "user:tester"))
	rec := httptest.NewRecorder()
	interactHandler(d).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}
