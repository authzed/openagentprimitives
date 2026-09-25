package agentui

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
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/web/browsersession"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uiview"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/browserstart"
)

// This file is package agentui (internal), not agentui_test, so it can call
// actionsHandler directly — the same shape bindings_test.go uses to test
// bindingsHandler without standing up a full webui.Server.

const (
	actionsNS      = "wsb"
	actionsSession = "sess-actions"
	actionsClass   = "class-actions"
	actionsUIName  = "ui-actions"
	actionsSubject = "user:dave"
	actionsOrigin  = "https://trusted.actions.example"

	// actionsDefaultState/actionsDefaultMessage are what actionsFakeDeps'
	// default NATSRequest stub replies with — the "runner accepted and is
	// working on it" outcome, distinct from the terminal "denied" outcome a
	// couple of cases below construct their own stub for.
	actionsDefaultState   = "submitted"
	actionsDefaultMessage = "Advancing the lead."
)

// actionsRootSlotJSON declares a control (ap:select, ParamProp "param")
// naming "lead" — the same binding-parameter vocabulary a data binding's
// args template draws on — plus an ap:button (ActionProp "action") naming
// the declared "advance" action below.
//
// The ap:daterange is load-bearing, not decoration: it is the ONLY registered
// component whose declared parameter NAME expands into several runtime KEYS
// ("window" -> "window.from"/"window.to"), so it is the only fixture shape
// that can tell uicomponents.ParamKeys from uicomponents.ParamNames. Without
// it, filtering this route's params against ParamNames looks identical to
// filtering against ParamKeys and the whole class of bug — every actions
// request from a page carrying a date range rejected wholesale — is free to
// come back. See TestActionsHandlerAcceptsAnExpandedParameterKey.
const actionsRootSlotJSON = `{
	"component":"ap:stack",
	"children":[
		{"component":"ap:select","props":{"param":"lead","value":"l-1"}},
		{"component":"ap:button","props":{"action":"advance","label":"Advance"}},
		{"component":"ap:daterange","props":{"param":"window","from":"2026-01-01","to":"2026-01-31"}}
	]
}`

// actionsAdvanceArgsJSON is "advance"'s args template: "lead" comes from the
// page's own selected-lead control (a binding parameter), "why" comes ONLY
// from this invocation's own form input — proving both value sources feed
// the SAME SubstituteParams call (actionsHandler step 8-9).
const actionsAdvanceArgsJSON = `{"lead":{"$param":"lead"},"why":{"$param":"why"}}`

func actionsFixtureSession() *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: actionsSession, Namespace: actionsNS},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: actionsClass},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
}

func actionsFixtureClass() *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: actionsClass, Namespace: actionsNS},
		Spec:       spiceboxv1alpha1.AgentClassSpec{AgentUI: &spiceboxv1alpha1.AgentClassUIGrant{Ref: actionsUIName}},
	}
}

// actionsFixtureUI builds the referenced AgentUI with one declared action
// ("advance", tool "crm_advance_lead", inputs ["why"]) and a Valid condition
// set per valid.
func actionsFixtureUI(valid bool) *spiceboxv1alpha1.AgentUI {
	status := metav1.ConditionFalse
	if valid {
		status = metav1.ConditionTrue
	}
	return &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: actionsUIName, Namespace: actionsNS},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Slots: []spiceboxv1alpha1.AgentUISlot{
				{Name: "root", Default: &apiextensionsv1.JSON{Raw: []byte(actionsRootSlotJSON)}},
			},
			Actions: []spiceboxv1alpha1.AgentUIAction{
				{
					Name:   "advance",
					Tool:   "crm_advance_lead",
					Args:   &apiextensionsv1.JSON{Raw: []byte(actionsAdvanceArgsJSON)},
					Inputs: []string{"why"},
				},
			},
		},
		Status: spiceboxv1alpha1.AgentUIStatus{
			Conditions: []metav1.Condition{{Type: spiceboxv1alpha1.AgentUIConditionValid, Status: status, Reason: "Test"}},
		},
	}
}

// actionsBaseObjs is a fresh Session+Class+UI(valid) fixture set — fresh per
// call since the fake client builder takes ownership of the objects it's
// given.
func actionsBaseObjs() []client.Object {
	return []client.Object{actionsFixtureSession(), actionsFixtureClass(), actionsFixtureUI(true)}
}

func newActionsFakeK8s(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// seededDeclaration converts actionsFixtureUI(true) through the SAME
// uiview.DeclarationFromSpec conversion resolveView/resolveDeclaration uses
// (viewmodel.go, via uiview.Resolve) — no fragments applied (this fixture's
// Tier 0 is what a fresh session sees) — so a control's action reference is
// read out of the fixture the endpoint actually resolves against, not
// hand-typed a second time. See TestActionsHandlerSpansTheDeclaredNameJoin.
func seededDeclaration(t *testing.T) uicomponents.Declaration {
	t.Helper()
	decl, err := uiview.DeclarationFromSpec(actionsFixtureUI(true))
	require.NoError(t, err)
	return decl
}

// actionsFakeDeps implements agentui.Deps for actions.go's own test suite,
// independent of bindingsFakeDeps (different collaborators exercised: this
// suite drives NATSRequest directly, never a uibindings.Resolver).
type actionsFakeDeps struct {
	interactOK  bool
	interactErr error
	k8s         client.Client
	origin      string

	// natsUnset simulates an unconfigured webd (no NATS wiring) — NATSRequest
	// returns nil, the same wiring-bug shape channelevents.RequestIn itself
	// guards against.
	natsUnset bool
	// nats, when set, REPLACES the default stub below. Used by cases that
	// need a specific runner-side outcome (a denial).
	nats channelevents.RequestFunc

	// gotRequest is the channelevents.UIActionRequest the stub NATSRequest
	// captured — what the runner was actually told to do.
	gotRequest channelevents.UIActionRequest
}

func (d *actionsFakeDeps) CheckInteract(context.Context, string, string, string) (bool, error) {
	return d.interactOK, d.interactErr
}
func (d *actionsFakeDeps) K8s() client.Client    { return d.k8s }
func (d *actionsFakeDeps) Logger() logr.Logger   { return logr.Discard() }
func (d *actionsFakeDeps) TrustedOrigin() string { return d.origin }

func (d *actionsFakeDeps) NATSRequest() channelevents.RequestFunc {
	if d.natsUnset {
		return nil
	}
	if d.nats != nil {
		return d.nats
	}
	return func(_ string, data []byte, _ time.Duration) ([]byte, error) {
		var env channelevents.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			return nil, err
		}
		var req channelevents.UIActionRequest
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			return nil, err
		}
		d.gotRequest = req
		return json.Marshal(channelevents.UIActionResponse{
			RequestID: req.RequestID, State: actionsDefaultState, Message: actionsDefaultMessage,
		})
	}
}

// Memory returns a fresh, always-empty fakeUIActionMemory (live_test.go) —
// resolveDeclaration (via resolveView/uiview.Resolve, viewmodel.go) now
// requires a non-nil backend to serve ANY declaration, Tier-0-only included.
// Every fixture in this file is Tier-0-only (no stored fragment), so an
// empty backend is exactly right; a nil Memory would 503 every case here.
func (d *actionsFakeDeps) Memory() memory.Memory                                   { return &fakeUIActionMemory{} }
func (d *actionsFakeDeps) Artifacts() *artifacts.Service                           { return nil }
func (d *actionsFakeDeps) ArtifactRenderBytes() uibindings.ArtifactRenderBytesFunc { return nil }
func (d *actionsFakeDeps) NATS() *nats.Conn                                        { return nil }

// StartBrowserSession is nil: no case in this file exercises the start
// route, only .../actions.
func (d *actionsFakeDeps) StartBrowserSession() browsersession.StartFunc { return nil }

// LiveSessions is nil for the same reason: only the start route reserves.
func (d *actionsFakeDeps) LiveSessions() browserstart.LiveSessions { return nil }
func (d *actionsFakeDeps) StartableNamespaces() []string           { return nil }
func (d *actionsFakeDeps) WorkshopNamespacesFor(context.Context, string) ([]string, error) {
	return nil, nil
}

var _ Deps = (*actionsFakeDeps)(nil)

// doPostAction builds and serves a POST .../actions request against
// actionsHandler(d). subject is injected into the request context only when
// non-empty (an empty subject simulates no authenticated cookie); the Origin
// header is set only when origin is non-empty.
func doPostAction(t *testing.T, d Deps, subject, origin, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/agent-ui/"+actionsNS+"/"+actionsSession+"/actions", strings.NewReader(body))
	req.SetPathValue("ns", actionsNS)
	req.SetPathValue("name", actionsSession)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if subject != "" {
		req = req.WithContext(webui.WithSubjectForTest(req.Context(), subject))
	}
	rec := httptest.NewRecorder()
	actionsHandler(d).ServeHTTP(rec, req)
	return rec
}

// recordedResponse is postAction's return shape: the HTTP status actually
// written, and the decoded body when it was JSON (every actionsHandler
// response is, on every status this suite exercises).
type recordedResponse struct {
	status int
	body   actionResponseBody
}

// postAction serves a request against a FIXED happy-path Deps (interactOK,
// valid UI, trusted origin, authenticated subject) and returns both the
// recorded response and the channelevents.UIActionRequest the stub
// NATSRequest captured — what the runner was actually told. Used by the
// join tests below, which vary only the request BODY.
func postAction(t *testing.T, body string) (recordedResponse, channelevents.UIActionRequest) {
	t.Helper()
	d := &actionsFakeDeps{interactOK: true, k8s: newActionsFakeK8s(t, actionsBaseObjs()...), origin: actionsOrigin}
	rec := doPostAction(t, d, actionsSubject, actionsOrigin, body)
	var got actionResponseBody
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), "body: %s", rec.Body.String())
	}
	return recordedResponse{status: rec.Code, body: got}, d.gotRequest
}

// TestActionsHandler table-drives the gate order: authentication, CSRF
// origin pin, CheckInteract (fail-closed on error, never a denial),
// malformed-body 400, an undeclared action name, an undeclared input, a
// missing param the args template needs, a nil/unconfigured NATSRequest
// (fail-closed 503, never a silent no-op), and both terminal runner
// outcomes (denied, submitted) landing as 200 so they render ON the control.
func TestActionsHandler(t *testing.T) {
	deniedStub := func(_ string, data []byte, _ time.Duration) ([]byte, error) {
		var env channelevents.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			return nil, err
		}
		var req channelevents.UIActionRequest
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			return nil, err
		}
		return json.Marshal(channelevents.UIActionResponse{
			RequestID: req.RequestID, State: "denied", Message: "You do not have permission to advance this lead.",
		})
	}

	cases := []struct {
		name        string
		subject     string
		origin      string
		interactOK  bool
		interactErr error
		natsUnset   bool
		nats        channelevents.RequestFunc
		body        string
		wantStatus  int
		check       func(t *testing.T, got actionResponseBody, sent channelevents.UIActionRequest)
	}{
		{name: "unauthenticated: 401", origin: actionsOrigin, wantStatus: http.StatusUnauthorized},
		{name: "wrong origin: 403 (this route mutates; CSRF is the boundary)",
			subject: actionsSubject, origin: "https://evil.example", wantStatus: http.StatusForbidden},
		{name: "interact errors: 503, never read as a denial",
			subject: actionsSubject, origin: actionsOrigin, interactErr: errors.New("spicedb down"),
			wantStatus: http.StatusServiceUnavailable},
		{name: "interact denies: 403", subject: actionsSubject, origin: actionsOrigin, wantStatus: http.StatusForbidden},
		{name: "malformed body: 400", subject: actionsSubject, origin: actionsOrigin, interactOK: true,
			body: "{not json", wantStatus: http.StatusBadRequest},
		{name: "an undeclared action name: 400, and nothing is dispatched",
			subject: actionsSubject, origin: actionsOrigin, interactOK: true,
			body: `{"action":"nope"}`, wantStatus: http.StatusBadRequest},
		// "why" is ALSO supplied here, satisfying the args template on its
		// own — so if the unknown "nonsense" key were silently dropped instead
		// of rejected, the request would otherwise succeed (200, submitted)
		// and this case would not catch it. Pairing the unknown key with an
		// otherwise-complete, valid request is what makes this case fail for
		// the RIGHT reason rather than coincidentally 400 because a required
		// value was missing.
		{name: "an undeclared input: 400, never silently dropped",
			subject: actionsSubject, origin: actionsOrigin, interactOK: true,
			body:       `{"action":"advance","params":{"lead":"l-1"},"inputs":{"why":"ready","nonsense":"x"}}`,
			wantStatus: http.StatusBadRequest},
		{name: "an undeclared param: 400, never silently dropped",
			subject: actionsSubject, origin: actionsOrigin, interactOK: true,
			body: `{"action":"advance","params":{"nonsense":"x"},"inputs":{"why":"ready"}}`, wantStatus: http.StatusBadRequest},
		{name: "a value the template needs was never supplied: 400, not a silently-empty substitution",
			subject: actionsSubject, origin: actionsOrigin, interactOK: true,
			body: `{"action":"advance","inputs":{"why":"ready"}}`, wantStatus: http.StatusBadRequest},
		{name: "a nil NATSRequest is 503, never a silent no-op button",
			subject: actionsSubject, origin: actionsOrigin, interactOK: true, natsUnset: true,
			body:       `{"action":"advance","params":{"lead":"l-1"},"inputs":{"why":"ready"}}`,
			wantStatus: http.StatusServiceUnavailable},
		{name: "a runner denial is a 200 carrying denied, so it renders ON the control",
			subject: actionsSubject, origin: actionsOrigin, interactOK: true, nats: deniedStub,
			body:       `{"action":"advance","params":{"lead":"l-1"},"inputs":{"why":"ready"}}`,
			wantStatus: http.StatusOK,
			check: func(t *testing.T, got actionResponseBody, _ channelevents.UIActionRequest) {
				assert.Equal(t, "denied", got.State)
				assert.NotEmpty(t, got.Message)
			}},
		{name: "happy path: submitted, and the runner is told the declared tool with substituted args",
			subject: actionsSubject, origin: actionsOrigin, interactOK: true,
			body:       `{"action":"advance","params":{"lead":"l-1"},"inputs":{"why":"ready"}}`,
			wantStatus: http.StatusOK,
			check: func(t *testing.T, got actionResponseBody, sent channelevents.UIActionRequest) {
				assert.Equal(t, actionsDefaultState, got.State)
				assert.NotEmpty(t, got.RequestID)
				assert.Equal(t, "crm_advance_lead", sent.ToolName)
				assert.Equal(t, "advance", sent.Action)
				assert.Equal(t, actionsSubject, sent.Requester)
				assert.JSONEq(t, `{"lead":"l-1","why":"ready"}`, string(sent.Args))
			}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &actionsFakeDeps{
				interactOK:  tc.interactOK,
				interactErr: tc.interactErr,
				k8s:         newActionsFakeK8s(t, actionsBaseObjs()...),
				origin:      actionsOrigin,
				natsUnset:   tc.natsUnset,
				nats:        tc.nats,
			}
			rec := doPostAction(t, d, tc.subject, tc.origin, tc.body)
			require.Equal(t, tc.wantStatus, rec.Code, "body: %s", rec.Body.String())
			if tc.check != nil {
				var got actionResponseBody
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
				tc.check(t, got, d.gotRequest)
			}
		})
	}
}

// TestActionsHandlerSpansTheDeclaredNameJoin owns J4: the name a control
// FIRES and the name the server LOOKS UP are the same string, and what the
// runner receives is the declaration's tool — not anything the browser
// said.
//
// The action name posted here is read out of the SAME declaration fixture
// the page renders (seededDeclaration), not hand-typed, so a rename on
// either side fails rather than producing a silently dead button.
func TestActionsHandlerSpansTheDeclaredNameJoin(t *testing.T) {
	decl := seededDeclaration(t)
	refs := uicomponents.ActionRefs(decl)
	require.NotEmpty(t, refs, "the fixture must contain a control that names an action")
	name := refs[0].Action

	declared, ok := decl.Action(name)
	require.True(t, ok, "a control naming an action the declaration lacks is the bug this spans")

	got, sent := postAction(t, `{"action":"`+name+`","params":{"lead":"l-1"},"inputs":{"why":"ready"}}`)
	assert.Equal(t, http.StatusOK, got.status, "body: %+v", got.body)
	assert.Equal(t, declared.Tool, sent.ToolName, "the runner is told the DECLARATION's tool")
	assert.Equal(t, name, sent.Action, "and the record is keyed by the DECLARED name")
}

// TestActionsHandlerNeverAcceptsAToolFromTheBrowser is the mutation-check
// TestActionsHandlerAcceptsAnExpandedParameterKey pins step 7's filter to
// uicomponents.ParamKeys. The browser's own parameter map is ALWAYS keyed by
// the expanded runtime key — an ap:daterange named "window" appears as
// "window.from"/"window.to" and never as "window" — and the whole request is
// rejected the moment any supplied key is unknown, so filtering against
// ParamNames instead rejects EVERY actions request from a page carrying a
// date range, action-relevant or not.
//
// TestFilterParams exercises the helper with a hand-supplied `declared` list,
// which cannot catch this: the defect is in the ARGUMENT, not the helper.
// Only a fixture whose declaration actually contains a compound-key control
// can tell the two calls apart.
func TestActionsHandlerAcceptsAnExpandedParameterKey(t *testing.T) {
	got, sent := postAction(t,
		`{"action":"advance","params":{"lead":"l-1","window.from":"2026-01-01","window.to":"2026-01-31"},"inputs":{"why":"ready"}}`)

	require.Equal(t, http.StatusOK, got.status,
		"an expanded parameter key is a legal key; rejecting it breaks every action on a page with a date range")
	assert.Equal(t, actionsDefaultState, got.body.State)
	assert.Equal(t, "advance", sent.Action)
}

// regression guard for invariant 1: a body naming its own tool must never be
// honored. actionRequestBody has no "tool" field, so an extra "tool" key in
// the JSON is simply not read — but if a future change added one and wired
// it in, the dispatched ToolName would equal the attacker-supplied value
// instead of the declaration's.
func TestActionsHandlerNeverAcceptsAToolFromTheBrowser(t *testing.T) {
	_, sent := postAction(t, `{"action":"advance","tool":"crm_delete_everything","params":{"lead":"l-1"},"inputs":{"why":"x"}}`)
	assert.NotEqual(t, "crm_delete_everything", sent.ToolName)
	assert.Equal(t, "crm_advance_lead", sent.ToolName, "the declaration's own tool must be used, never anything from the request body")
}

// TestActionsHandlerNeverLeaksInternalIdentifiers asserts every response
// body across a spread of error-triggering requests carries no internal
// operational vocabulary — a CRD kind, a kubectl-shaped path, a SpiceDB
// relation, a memory kind, or the tool name.
func TestActionsHandlerNeverLeaksInternalIdentifiers(t *testing.T) {
	forbidden := []string{"AgentUI", "AgentSession", "kubectl", "ap.session.", "ui_action", "spicedb", "memory.Scope", "crm_advance_lead"}

	cases := []struct {
		name string
		d    *actionsFakeDeps
		body string
	}{
		{
			name: "interact errors",
			d: &actionsFakeDeps{interactErr: errors.New("spicedb: dial tcp 10.0.0.1:443: connect: connection refused"),
				k8s: newActionsFakeK8s(t, actionsBaseObjs()...), origin: actionsOrigin},
			body: `{"action":"advance"}`,
		},
		{
			name: "session not found",
			d: &actionsFakeDeps{interactOK: true,
				k8s: newActionsFakeK8s(t, actionsFixtureClass(), actionsFixtureUI(true)), origin: actionsOrigin},
			body: `{"action":"advance"}`,
		},
		{
			name: "AgentUI not yet valid",
			d: &actionsFakeDeps{interactOK: true,
				k8s: newActionsFakeK8s(t, actionsFixtureSession(), actionsFixtureClass(), actionsFixtureUI(false)), origin: actionsOrigin},
			body: `{"action":"advance"}`,
		},
		{
			name: "undeclared action",
			d: &actionsFakeDeps{interactOK: true,
				k8s: newActionsFakeK8s(t, actionsBaseObjs()...), origin: actionsOrigin},
			body: `{"action":"nope"}`,
		},
		{
			name: "undeclared input",
			d: &actionsFakeDeps{interactOK: true,
				k8s: newActionsFakeK8s(t, actionsBaseObjs()...), origin: actionsOrigin},
			body: `{"action":"advance","params":{"lead":"l-1"},"inputs":{"nonsense":"x"}}`,
		},
		{
			name: "malformed body",
			d: &actionsFakeDeps{interactOK: true,
				k8s: newActionsFakeK8s(t, actionsBaseObjs()...), origin: actionsOrigin},
			body: `{not json`,
		},
		{
			name: "nats unconfigured",
			d: &actionsFakeDeps{interactOK: true, natsUnset: true,
				k8s: newActionsFakeK8s(t, actionsBaseObjs()...), origin: actionsOrigin},
			body: `{"action":"advance","params":{"lead":"l-1"},"inputs":{"why":"x"}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doPostAction(t, tc.d, actionsSubject, actionsOrigin, tc.body)
			body := rec.Body.String()
			for _, f := range forbidden {
				assert.NotContains(t, body, f, "case %q: response leaked an internal identifier", tc.name)
			}
		})
	}
}

// TestFilterActionInputs unit-tests the exact-match rule (actionsHandler
// step 7) independent of the HTTP layer. Unlike filterParams (bindings.go),
// there is no compound-key expansion: each declared Input name IS the
// literal key.
func TestFilterActionInputs(t *testing.T) {
	declared := []string{"why", "note"}
	cases := []struct {
		name        string
		supplied    map[string]string
		wantKept    map[string]string
		wantUnknown []string
	}{
		{
			name:     "exact match kept",
			supplied: map[string]string{"why": "ready"},
			wantKept: map[string]string{"why": "ready"},
		},
		{
			name:        "unrelated key rejected alongside a valid one",
			supplied:    map[string]string{"why": "ready", "nonsense": "x"},
			wantKept:    map[string]string{"why": "ready"},
			wantUnknown: []string{"nonsense"},
		},
		{
			// No compound expansion for inputs (unlike a binding parameter's
			// declared name expanding through ParamKeys): a prefix-shaped key is
			// just an unrelated key.
			name:        "no compound expansion: a dotted key is not a match",
			supplied:    map[string]string{"why.detail": "x"},
			wantKept:    map[string]string{},
			wantUnknown: []string{"why.detail"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kept, unknown := filterActionInputs(tc.supplied, declared)
			assert.Equal(t, tc.wantKept, kept)
			assert.ElementsMatch(t, tc.wantUnknown, unknown)
		})
	}
}
