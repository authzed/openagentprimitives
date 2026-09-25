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

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/artifactview"
)

// appToolCallFakeDeps is a hand-rolled Deps for testing appToolCallHandler's
// gate matrix, independent of fakeDeps in handlers_test.go (which the
// /interact tests own) — the /app-tool-call surface exercises CheckInteract +
// AgentClassOf + NATSRequest + TrustedOrigin + Logger, never
// CheckArtifactView (there is no artifact in an app tool call), so a separate
// minimal fake keeps each suite's fields meaningful.
type appToolCallFakeDeps struct {
	// noSubject, when true, simulates an unauthenticated caller (no subject
	// injected into the request context).
	noSubject bool

	// interact is CheckInteract's ok return; interactErr forces it to error
	// instead (simulating a SpiceDB outage — must fail closed as 503).
	interact    bool
	interactErr bool

	// class is the AgentClass AgentClassOf resolves. The zero value (nil)
	// means "a class that grants session_views", so the rows below that are
	// not about this gate read unchanged; classNoViews and classErr select
	// the two failure shapes.
	class        *spiceboxv1alpha1.AgentClass
	classNoViews bool
	classErr     bool

	// nats, when set, replaces the default NATSRequest stub. A nil value
	// (the zero value of channelevents.RequestFunc) simulates the
	// nil-NATSRequest wiring bug.
	nats           channelevents.RequestFunc
	natsUnset      bool
	gotRequestBody []byte // raw envelope bytes RequestIn's transport received

	// widgetOrigins stands in for the session's status.activeWidgets:
	// artifactID -> the MCPServer origin that produced that widget. An id
	// absent from the map is a widget this session does not have.
	// widgetOriginErr forces the lookup to error instead (a status-read
	// outage, which must fail closed rather than proceed unpinned).
	widgetOrigins     map[string]string
	widgetOriginErr   bool
	widgetOriginCalls int
}

func (d *appToolCallFakeDeps) WidgetOriginOf(_ context.Context, _, _, artifactID string) (string, bool, error) {
	d.widgetOriginCalls++
	if d.widgetOriginErr {
		return "", false, errors.New("k8s: simulated status read failure")
	}
	origin, ok := d.widgetOrigins[artifactID]
	return origin, ok, nil
}

func (d *appToolCallFakeDeps) CheckInteract(_ context.Context, _, _, _ string) (bool, error) {
	if d.interactErr {
		return false, errors.New("spicedb: simulated outage")
	}
	return d.interact, nil
}

// CheckArtifactView and ListRevisions both fail loudly here, and unlike the
// AgentClassOf assertion this one is genuine rather than a pinned bug: an app
// tool call carries no artifact id at all (appToolCallRequestBody is toolName
// + args), so there is no artifact reference to authorize and no
// artifact→session binding to resolve. This route's authorization is
// session-scoped end to end.
func (d *appToolCallFakeDeps) CheckArtifactView(_ context.Context, _, _ string) (bool, error) {
	return false, errors.New("app-tool-call must never call CheckArtifactView: it references no artifact")
}

func (d *appToolCallFakeDeps) ListRevisions(_ context.Context, _, _, _ string) ([]artifactview.RevisionMeta, error) {
	return nil, errors.New("app-tool-call must never call ListRevisions: it references no artifact")
}

// AgentClassOf resolves the class whose session_views grant gates this route.
// It previously returned errors.New("app-tool-call must never call
// AgentClassOf") — an assertion that pinned the MISSING class opt-in as
// intended behavior. /interact requires the grant before dispatching
// (handlers.go step 5); this route must too, so the fake now serves a class.
func (d *appToolCallFakeDeps) AgentClassOf(_ context.Context, _, _ string) (*spiceboxv1alpha1.AgentClass, error) {
	if d.classErr {
		return nil, errors.New("agentclass: simulated not-found")
	}
	if d.classNoViews {
		return classNoCaps(), nil
	}
	if d.class != nil {
		return d.class, nil
	}
	return classWithSessionViews("user_message"), nil
}

func (d *appToolCallFakeDeps) NATSRequest() channelevents.RequestFunc {
	if d.natsUnset {
		return nil
	}
	if d.nats != nil {
		return d.nats
	}
	return func(_ string, data []byte, _ time.Duration) ([]byte, error) {
		d.gotRequestBody = data
		return json.Marshal(channelevents.AppToolCallResponse{Status: channelevents.AppToolCallStatusOK,
			Result: json.RawMessage(`{"ok":true}`)})
	}
}

func (d *appToolCallFakeDeps) TrustedOrigin() string { return testTrustedOrigin }
func (d *appToolCallFakeDeps) Logger() logr.Logger   { return logr.Discard() }

// postAppToolCall builds and serves a POST /session/ns1/sess1/app-tool-call
// request against appToolCallHandler(d), returning the recorded response.
// Injects an authenticated subject unless d.noSubject is set, and a trusted
// Origin header (the CSRF gate is not what most rows below are testing).
func postAppToolCall(t *testing.T, d *appToolCallFakeDeps, subject, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/session/ns1/sess1/app-tool-call", strings.NewReader(body))
	req.SetPathValue("ns", "ns1")
	req.SetPathValue("name", "sess1")
	req.Header.Set("Origin", testTrustedOrigin)
	if !d.noSubject {
		req = req.WithContext(webui.WithSubjectForTest(req.Context(), subject))
	}
	rec := httptest.NewRecorder()
	appToolCallHandler(d).ServeHTTP(rec, req)
	return rec
}

func TestAppToolCallAllowedRoundTripsToRunnerWithVerifiedRequester(t *testing.T) {
	d := &appToolCallFakeDeps{interact: true}
	// The body smuggles a bogus "requester" — the handler must ignore it and
	// use the cookie-verified subject instead.
	rec := postAppToolCall(t, d, "user:viewer",
		`{"toolName":"widget_tool","args":{"x":1},"requester":"user:attacker"}`)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp channelevents.AppToolCallResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, channelevents.AppToolCallStatusOK, resp.Status)
	assert.JSONEq(t, `{"ok":true}`, string(resp.Result))

	// Assert the request the fake responder actually received: server-verified
	// Requester and the browser's toolName/args, never the body's requester.
	require.NotNil(t, d.gotRequestBody, "runner responder must have been invoked")
	var env channelevents.Envelope
	require.NoError(t, json.Unmarshal(d.gotRequestBody, &env))
	var sent channelevents.AppToolCallRequest
	require.NoError(t, json.Unmarshal(env.Payload, &sent))
	assert.Equal(t, "widget_tool", sent.ToolName)
	assert.JSONEq(t, `{"x":1}`, string(sent.Args))
	assert.Equal(t, "user:viewer", sent.Requester, "Requester must be the cookie-verified subject, not the body's")
	assert.NotEmpty(t, sent.RequestID, "server must mint a RequestID")
}

func TestAppToolCallCheckInteractDeniedIsForbiddenAndNeverCallsNATS(t *testing.T) {
	d := &appToolCallFakeDeps{interact: false, nats: func(_ string, _ []byte, _ time.Duration) ([]byte, error) {
		t.Fatal("must not reach the NATS transport when CheckInteract denies")
		return nil, nil
	}}
	rec := postAppToolCall(t, d, "user:viewer", `{"toolName":"widget_tool","args":{}}`)
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
}

func TestAppToolCallCheckInteractErrorsFailsClosedAsServiceUnavailable(t *testing.T) {
	d := &appToolCallFakeDeps{interactErr: true, nats: func(_ string, _ []byte, _ time.Duration) ([]byte, error) {
		t.Fatal("must not reach the NATS transport when CheckInteract errors")
		return nil, nil
	}}
	rec := postAppToolCall(t, d, "user:viewer", `{"toolName":"widget_tool","args":{}}`)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
}

// TestAppToolCallRequiresSessionViewsClassOptIn pins the class opt-in this
// route shares with /interact (handlers.go step 5). CheckInteract answers "may
// this subject talk to this session at all"; session_views answers the
// independent question "does THIS class permit any browser-view interaction in
// the first place" — it is opt-in (defaultOn=false). Without it, on a class
// that never enabled browser interaction, any session participant could invoke
// MCP app-visible tools directly, and a readonly tool auto-runs with no
// approval. The runner re-checks and contains the blast radius, which makes
// this defense in depth rather than the only gate — but the two sibling routes
// must not disagree about a capability either one of them enforces.
func TestAppToolCallRequiresSessionViewsClassOptIn(t *testing.T) {
	neverNATS := func(t *testing.T) channelevents.RequestFunc {
		return func(_ string, _ []byte, _ time.Duration) ([]byte, error) {
			t.Fatal("must not reach the NATS transport when the class opt-in gate refuses")
			return nil, nil
		}
	}
	cases := []struct {
		name       string
		mutate     func(d *appToolCallFakeDeps)
		wantStatus int
	}{
		{
			name:       "session_views granted → 200, dispatches to the runner",
			mutate:     func(d *appToolCallFakeDeps) {},
			wantStatus: http.StatusOK,
		},
		{
			name:       "session_views absent from the class → 403, never reaches the runner",
			mutate:     func(d *appToolCallFakeDeps) { d.classNoViews = true; d.nats = neverNATS(t) },
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "AgentClassOf errors → 403 (fail closed), never reaches the runner",
			mutate:     func(d *appToolCallFakeDeps) { d.classErr = true; d.nats = neverNATS(t) },
			wantStatus: http.StatusForbidden,
		},
		{
			name: "session_views granted with an empty interaction list → 200 (app tool calls are not an interaction kind)",
			mutate: func(d *appToolCallFakeDeps) {
				d.class = classWithSessionViews()
			},
			wantStatus: http.StatusOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &appToolCallFakeDeps{interact: true}
			tc.mutate(d)
			rec := postAppToolCall(t, d, "user:viewer", `{"toolName":"widget_tool","args":{}}`)
			assert.Equal(t, tc.wantStatus, rec.Code, "body: %s", rec.Body.String())
		})
	}
}

func TestAppToolCallNotAuthenticatedIsUnauthorized(t *testing.T) {
	d := &appToolCallFakeDeps{interact: true, noSubject: true}
	rec := postAppToolCall(t, d, "", `{"toolName":"widget_tool","args":{}}`)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "body: %s", rec.Body.String())
}

func TestAppToolCallOriginMismatchIsForbidden(t *testing.T) {
	d := &appToolCallFakeDeps{interact: true}
	req := httptest.NewRequest(http.MethodPost, "/session/ns1/sess1/app-tool-call",
		strings.NewReader(`{"toolName":"widget_tool","args":{}}`))
	req.SetPathValue("ns", "ns1")
	req.SetPathValue("name", "sess1")
	req.Header.Set("Origin", "https://evil.example")
	req = req.WithContext(webui.WithSubjectForTest(req.Context(), "user:viewer"))
	rec := httptest.NewRecorder()
	appToolCallHandler(d).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestAppToolCallMalformedBodyIsBadRequest(t *testing.T) {
	d := &appToolCallFakeDeps{interact: true}
	rec := postAppToolCall(t, d, "user:viewer", `not json`)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
}

func TestAppToolCallEmptyToolNameIsBadRequest(t *testing.T) {
	d := &appToolCallFakeDeps{interact: true}
	rec := postAppToolCall(t, d, "user:viewer", `{"toolName":"","args":{}}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
}

func TestAppToolCallRunnerTimeoutReturns200WithErrorStatus(t *testing.T) {
	d := &appToolCallFakeDeps{interact: true, nats: func(_ string, _ []byte, _ time.Duration) ([]byte, error) {
		return nil, errors.New("nats: timeout")
	}}
	rec := postAppToolCall(t, d, "user:viewer", `{"toolName":"widget_tool","args":{}}`)

	require.Equal(t, http.StatusOK, rec.Code, "a transport failure must never hang or 5xx the browser")
	var resp channelevents.AppToolCallResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, channelevents.AppToolCallStatusError, resp.Status)
	// ViewerMessage, not Message: this handler authored the copy, so it goes in
	// the field a replying site vouches for. Message is the operator channel and
	// is stripped before the browser -- see apptoolcall_message_leak_test.go.
	assert.NotEmpty(t, resp.ViewerMessage)
	assert.Empty(t, resp.Message)
}

func TestAppToolCallNilNATSRequestReturns200WithErrorStatusNotAPanic(t *testing.T) {
	d := &appToolCallFakeDeps{interact: true, natsUnset: true}
	require.NotPanics(t, func() {
		rec := postAppToolCall(t, d, "user:viewer", `{"toolName":"widget_tool","args":{}}`)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		var resp channelevents.AppToolCallResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, channelevents.AppToolCallStatusError, resp.Status)
	})
}

func TestAppToolCallMalformedRunnerReplyReturns200WithErrorStatus(t *testing.T) {
	d := &appToolCallFakeDeps{interact: true, nats: func(_ string, _ []byte, _ time.Duration) ([]byte, error) {
		return []byte("not json"), nil
	}}
	rec := postAppToolCall(t, d, "user:viewer", `{"toolName":"widget_tool","args":{}}`)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp channelevents.AppToolCallResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, channelevents.AppToolCallStatusError, resp.Status)
}

// TestAppToolCallRelaysNonOKRunnerStatusesVerbatim pins the HTTP status
// policy: every runner-reported outcome (denied, requires_approval,
// not_found, rate_limited) is relayed at HTTP 200 — the widget reads
// resp.Status, never the HTTP status code, to decide what happened.
func TestAppToolCallRelaysNonOKRunnerStatusesVerbatim(t *testing.T) {
	statuses := []string{
		channelevents.AppToolCallStatusDenied,
		channelevents.AppToolCallStatusRequiresApproval,
		channelevents.AppToolCallStatusNotFound,
		channelevents.AppToolCallStatusRateLimited,
	}
	for _, status := range statuses {
		t.Run(status, func(t *testing.T) {
			d := &appToolCallFakeDeps{interact: true, nats: func(_ string, _ []byte, _ time.Duration) ([]byte, error) {
				return json.Marshal(channelevents.AppToolCallResponse{Status: status, Message: "reason"})
			}}
			rec := postAppToolCall(t, d, "user:viewer", `{"toolName":"widget_tool","args":{}}`)
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			var resp channelevents.AppToolCallResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, status, resp.Status)
		})
	}
}
