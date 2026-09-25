package agentui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
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
	"github.com/authzed/openagentprimitives/pkg/authz/toolguard"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/web/browsersession"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings/registry"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/browserstart"
)

// This file is package agentui (internal), not agentui_test, so it can call
// bindingsHandler directly — the same shape pkg/web/webui/interact's
// apptoolcall_test.go uses to test appToolCallHandler without standing up a
// full webui.Server. page_test.go/agentui_test.go stay agentui_test because
// they exercise the route set — the redirect included — through the real
// Server/Routes() wiring.

const (
	bindingsNS      = "wsb"
	bindingsSession = "sess-bindings"
	bindingsClass   = "class-bindings"
	bindingsUIName  = "ui-bindings"
	bindingsSubject = "user:carol"
	bindingsOrigin  = "https://trusted.bindings.example"
)

// bindingsFakeDeps implements agentui.Deps for bindings.go's own test suite,
// independent of agentui_test.go's fakeDeps (different package, and this
// suite only exercises CheckInteract/K8s/TrustedOrigin/Logger — the
// remaining uibindings.Deps collaborators are never touched because every
// case below routes through the "tool" source via fakeToolResolver, never
// the real memory/artifact resolvers).
type bindingsFakeDeps struct {
	interactOK  bool
	interactErr error
	k8s         client.Client
	origin      string
	// logSink, when non-nil, receives every Logger().Info call as a
	// "<msg> <k1>=<v1> <k2>=<v2>…" line — the same funcr-over-strings.Builder
	// shape viewmodel_test.go's vmLogSink already uses in this package. The
	// selector's operator-facing diagnostic (segment, index, element, kind,
	// found) is a REQUIRED surface of the match gate, not incidental output,
	// so tests that assert on it need it recorded; every other test leaves
	// this nil and gets the original logr.Discard() behavior unchanged.
	logSink *strings.Builder
}

func (d *bindingsFakeDeps) CheckInteract(context.Context, string, string, string) (bool, error) {
	return d.interactOK, d.interactErr
}
func (d *bindingsFakeDeps) K8s() client.Client { return d.k8s }
func (d *bindingsFakeDeps) Logger() logr.Logger {
	if d.logSink == nil {
		return logr.Discard()
	}
	return funcr.New(func(prefix, args string) {
		d.logSink.WriteString(prefix)
		d.logSink.WriteString(" ")
		d.logSink.WriteString(args)
		d.logSink.WriteString("\n")
	}, funcr.Options{})
}
func (d *bindingsFakeDeps) TrustedOrigin() string                  { return d.origin }
func (d *bindingsFakeDeps) NATSRequest() channelevents.RequestFunc { return nil }

// Memory returns a fresh, always-empty fakeUIActionMemory (live_test.go) —
// resolveDeclaration (via resolveView/uiview.Resolve, viewmodel.go) now
// requires a non-nil backend to serve ANY declaration, Tier-0-only included.
// Every fixture in this file is Tier-0-only (no stored fragment), so an
// empty backend is exactly right; a nil Memory would 503 every case here.
func (d *bindingsFakeDeps) Memory() memory.Memory                                   { return &fakeUIActionMemory{} }
func (d *bindingsFakeDeps) Artifacts() *artifacts.Service                           { return nil }
func (d *bindingsFakeDeps) ArtifactRenderBytes() uibindings.ArtifactRenderBytesFunc { return nil }
func (d *bindingsFakeDeps) NATS() *nats.Conn                                        { return nil }

// StartBrowserSession is nil: no case in this file exercises the start
// route, only .../bindings.
func (d *bindingsFakeDeps) StartBrowserSession() browsersession.StartFunc { return nil }

// LiveSessions is nil for the same reason: only the start route reserves.
func (d *bindingsFakeDeps) LiveSessions() browserstart.LiveSessions { return nil }
func (d *bindingsFakeDeps) StartableNamespaces() []string           { return nil }
func (d *bindingsFakeDeps) WorkshopNamespacesFor(context.Context, string) ([]string, error) {
	return nil, nil
}

var _ Deps = (*bindingsFakeDeps)(nil)

// fakeToolResolver is a controllable "tool" uibindings.Resolver, registered
// (and reset) per test — never the real pkg/web/uibindings/tool, which needs a
// live runner over NATS. Ref selects the outcome: "list_rows" succeeds,
// "fail_ref" fails, "envelope_rows"/"empty_envelope"/"large_field" exist for
// the selector tests below, anything else falls through to a generic failure
// — mirroring how a real resolver never recognizes an arbitrary ref.
//
// calls counts Resolve invocations, so a test can prove the resolver was
// NEVER reached — see TestBindingsHandlerRefusesAnUnparseableSelector, which
// exists to catch an implementation that parses the selector AFTER resolving
// (hammering the upstream on every render of a page with one broken
// selector). A pointer receiver, not a value one, because the counter must
// be shared with whatever the registry holds — registry.Register keeps
// exactly the value it was given, so the test that reads calls must be
// reading the SAME instance Resolve was invoked on.
type fakeToolResolver struct {
	calls atomic.Int32
}

func (r *fakeToolResolver) Source() string { return "tool" }

func (r *fakeToolResolver) Resolve(_ context.Context, _ uibindings.Deps, req uibindings.Request) (uibindings.Result, error) {
	r.calls.Add(1)
	switch req.Ref {
	case "list_rows":
		return uibindings.Result{Value: json.RawMessage(`[{"a":1}]`)}, nil
	case "golden_rows":
		// The ref ui/testdata/bindings.golden.json's bound ap:table names. Its
		// value is asserted, cell for cell, by the frontend suite rendering
		// that same golden — see bindings_golden_internal_test.go.
		return uibindings.Result{Value: json.RawMessage(`[{"name":"Acme"},{"name":"Globex"}]`)}, nil
	case "golden_envelope":
		// The golden's OWN envelope-shaped ref, distinct from envelope_rows
		// below (the selector tests' own unit-test ref): a change to either
		// cannot silently move the other. total/paging are load-bearing, not
		// decoration — they are what makes the golden's extracted `data/#rows`
		// value differ from the raw bytes, so an implementation that skipped
		// extraction would produce a response the golden test can see is wrong.
		return uibindings.Result{Value: json.RawMessage(`{"results":[{"properties":{"name":"Initech"}},{"properties":{"name":"Umbrella"}}],"total":2,"paging":{"next":"c1"}}`)}, nil
	case "envelope_rows":
		// A real endpoint's envelope shape: fields nested under ".properties",
		// with sibling keys ("total") a selector must not surface. Used by the
		// selector match/miss tests below.
		return uibindings.Result{Value: json.RawMessage(`{"results":[{"properties":{"name":"Acme"}},{"properties":{"name":"Globex"}}],"total":2}`)}, nil
	case "empty_envelope":
		return uibindings.Result{Value: json.RawMessage(`{"results":[]}`)}, nil
	case "fail_ref":
		return uibindings.Result{}, errors.New("this action failed")
	default:
		return uibindings.Result{}, errors.New("this view's data could not be loaded")
	}
}

// registerFakeToolResolver resets the process-wide uibindings registry and
// registers ONLY fakeToolResolver, so "/2#text"'s bogus_source binding
// (below) genuinely misses the registry rather than hitting a real resolver.
// Returns the registered instance so a caller that needs its call counter
// (the unparseable-selector ordering test) can read it; every other caller
// ignores the return value, which Go allows without an explicit discard.
func registerFakeToolResolver(t *testing.T) *fakeToolResolver {
	t.Helper()
	registry.Reset()
	t.Cleanup(registry.Reset)
	r := &fakeToolResolver{}
	registry.Register(r)
	return r
}

// rootSlotJSON declares a control (ap:select, ParamProp "param") naming
// "span", plus a bound ap:table.rows whose args template references
// {"$param":"span"} — WalkBindings yields path "/0.1#rows": this "root" slot
// is not agentWritable, so CompileSlots places its default directly in the
// page's own root region (no hook wraps it) as the outer stack's child 0,
// and the bound ap:table sits at that node's own child 1.
//
// ap:select rather than ap:daterange because ap:select's registered
// ParamValues expand "span" to the single runtime key "span", which is what
// {"$param":"span"} needs. An ap:daterange naming "span" would drive
// "span.from"/"span.to" and never "span" — a declaration uicomponents.Validate
// now rejects outright (see TestValidateRejectsAnUnsatisfiableParamReference).
// The trailing ap:daterange drives NO binding of its own — it is here purely
// so this fixture's declaration contains the one registered component whose
// declared parameter NAME expands into several runtime KEYS
// ("window" -> "window.from"/"window.to"). It is what makes step 6's
// uicomponents.ParamKeys(decl) call distinguishable from ParamNames(decl):
// with only single-key controls the two are identical, and filtering against
// ParamNames — which rejects every request from a page carrying a date
// range — reads as correct. It is appended LAST so the existing
// WalkBindings paths ("/0.1#rows") are unchanged. See
// TestBindingsHandlerAcceptsAnExpandedParameterKey.
const rootSlotJSON = `{
	"component":"ap:stack",
	"children":[
		{"component":"ap:select","props":{"param":"span","value":"7d"}},
		{"component":"ap:table","bindings":{"rows":{"source":"tool","ref":"list_rows","args":{"span":{"$param":"span"}}}}},
		{"component":"ap:daterange","props":{"param":"window","from":"2026-01-01","to":"2026-01-31"}}
	]
}`

// sideSlotJSON's own root node carries the bound prop directly (no
// children); it is not agentWritable, so it sits directly at the outer
// root's child 1, and WalkBindings yields path "/1#body". ref "fail_ref"
// always errors — used to prove one binding's failure never fails the
// request.
const sideSlotJSON = `{"component":"ap:alert","bindings":{"body":{"source":"tool","ref":"fail_ref","args":{}}}}`

// oddSlotJSON binds to a source no resolver is registered for — path
// "/2#text" (the outer root's child 2) — proving a missing resolver errors
// on that binding only.
const oddSlotJSON = `{"component":"ap:text","bindings":{"text":{"source":"bogus_source","ref":"whatever","args":{}}}}`

func bindingsFixtureSession() *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: bindingsSession, Namespace: bindingsNS},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: bindingsClass},
		Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: spiceboxv1alpha1.AgentSessionPhaseRunning},
	}
}

func bindingsFixtureClass() *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: bindingsClass, Namespace: bindingsNS},
		Spec:       spiceboxv1alpha1.AgentClassSpec{AgentUI: &spiceboxv1alpha1.AgentClassUIGrant{Ref: bindingsUIName}},
	}
}

// bindingsFixtureUI builds the referenced AgentUI with the root/side/odd
// slots above and a Valid condition set per valid.
func bindingsFixtureUI(valid bool) *spiceboxv1alpha1.AgentUI {
	status := metav1.ConditionFalse
	if valid {
		status = metav1.ConditionTrue
	}
	return &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: bindingsUIName, Namespace: bindingsNS},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Slots: []spiceboxv1alpha1.AgentUISlot{
				{Name: "root", Default: &apiextensionsv1.JSON{Raw: []byte(rootSlotJSON)}},
				{Name: "side", Default: &apiextensionsv1.JSON{Raw: []byte(sideSlotJSON)}},
				{Name: "odd", Default: &apiextensionsv1.JSON{Raw: []byte(oddSlotJSON)}},
			},
		},
		Status: spiceboxv1alpha1.AgentUIStatus{
			Conditions: []metav1.Condition{{Type: spiceboxv1alpha1.AgentUIConditionValid, Status: status, Reason: "Test"}},
		},
	}
}

// bindingsBaseObjs is a fresh Session+Class+UI(valid) fixture set — fresh
// per call since the fake client builder takes ownership of the objects it's
// given.
func bindingsBaseObjs() []client.Object {
	return []client.Object{bindingsFixtureSession(), bindingsFixtureClass(), bindingsFixtureUI(true)}
}

func newBindingsFakeK8s(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// selectorFixtureUI builds a SINGLE-slot AgentUI ("sel", not agentWritable)
// whose only bound prop is an ap:table.rows naming ref with the given
// selector — path "/0#rows" (the outer root's sole child). Kept entirely
// separate from bindingsFixtureUI/rootSlotJSON/sideSlotJSON rather than
// grafted onto them: those two have documented WalkBindings paths
// ("/0.1#rows", "/1#body") other tests assert exactly, and the selector
// tests below have no reason to risk perturbing them — the same reasoning
// task-3-brief gives for not editing rootSlotJSON/sideSlotJSON directly.
func selectorFixtureUI(t *testing.T, ref, sel string) *spiceboxv1alpha1.AgentUI {
	t.Helper()
	node := struct {
		Component string                    `json:"component"`
		Props     map[string]any            `json:"props"`
		Bindings  map[string]map[string]any `json:"bindings"`
	}{
		Component: "ap:table",
		Props: map[string]any{
			"columns": []map[string]string{{"key": "name", "header": "Name"}},
			"rows":    []any{},
		},
		Bindings: map[string]map[string]any{
			"rows": {"source": "tool", "ref": ref, "args": map[string]any{}, "select": sel},
		},
	}
	raw, err := json.Marshal(node)
	require.NoError(t, err)
	return &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: bindingsUIName, Namespace: bindingsNS},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Slots: []spiceboxv1alpha1.AgentUISlot{
				{Name: "sel", Default: &apiextensionsv1.JSON{Raw: raw}},
			},
		},
		Status: spiceboxv1alpha1.AgentUIStatus{
			Conditions: []metav1.Condition{{Type: spiceboxv1alpha1.AgentUIConditionValid, Status: metav1.ConditionTrue, Reason: "Test"}},
		},
	}
}

// selectorFixtureObjs is a fresh Session+Class+UI(selectorFixtureUI) set.
func selectorFixtureObjs(t *testing.T, ref, sel string) []client.Object {
	t.Helper()
	return []client.Object{bindingsFixtureSession(), bindingsFixtureClass(), selectorFixtureUI(t, ref, sel)}
}

// postBindings builds and serves a POST .../bindings request against
// bindingsHandler(d). subject is injected into the request context only
// when non-empty (an empty subject simulates no authenticated cookie); the
// Origin header is set only when origin is non-empty.
func postBindings(t *testing.T, d Deps, subject, origin, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/agent-ui/"+bindingsNS+"/"+bindingsSession+"/bindings", strings.NewReader(body))
	req.SetPathValue("ns", bindingsNS)
	req.SetPathValue("name", bindingsSession)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if subject != "" {
		req = req.WithContext(webui.WithSubjectForTest(req.Context(), subject))
	}
	rec := httptest.NewRecorder()
	bindingsHandler(d).ServeHTTP(rec, req)
	return rec
}

// TestBindingsHandler table-drives the gate order plus the per-binding
// resolution behavior: authentication, CSRF origin pin, CheckInteract
// (fail-closed on error, never a denial), malformed-body 400, the
// undeclared-parameter request-level rejection (the carry-forward from
// Task 3 — see bindingsHandler's step-6 doc comment for why this belongs at
// the fan-out caller and not in SubstituteParams), and the happy/partial-
// failure paths proving one binding's failure never fails the request.
func TestBindingsHandler(t *testing.T) {
	registerFakeToolResolver(t)

	cases := []struct {
		name        string
		subject     string
		origin      string
		interactOK  bool
		interactErr error
		body        string
		wantStatus  int
		check       func(t *testing.T, got bindingsResponseBody)
	}{
		{
			name:       "unauthenticated: 401",
			origin:     bindingsOrigin,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong origin: 403",
			subject:    bindingsSubject,
			origin:     "https://evil.example",
			interactOK: true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:        "interact errors: 503, never read as a denial",
			subject:     bindingsSubject,
			origin:      bindingsOrigin,
			interactErr: errors.New("spicedb down"),
			wantStatus:  http.StatusServiceUnavailable,
		},
		{
			name:       "interact denies: 403",
			subject:    bindingsSubject,
			origin:     bindingsOrigin,
			interactOK: false,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "malformed body: 400",
			subject:    bindingsSubject,
			origin:     bindingsOrigin,
			interactOK: true,
			body:       "{not json",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "happy path: every declared binding is resolved and keyed by its path",
			subject:    bindingsSubject,
			origin:     bindingsOrigin,
			interactOK: true,
			body:       `{"params":{"span":"30d"}}`,
			wantStatus: http.StatusOK,
			check: func(t *testing.T, got bindingsResponseBody) {
				require.Contains(t, got.Bindings, "/0.1#rows")
				assert.Equal(t, "ok", got.Bindings["/0.1#rows"].Status)
				assert.JSONEq(t, `[{"a":1}]`, string(got.Bindings["/0.1#rows"].Value))
			},
		},
		{
			name:       "one failing binding does not fail the page",
			subject:    bindingsSubject,
			origin:     bindingsOrigin,
			interactOK: true,
			body:       `{"params":{"span":"30d"}}`,
			wantStatus: http.StatusOK,
			check: func(t *testing.T, got bindingsResponseBody) {
				assert.Equal(t, "error", got.Bindings["/1#body"].Status)
				assert.NotEmpty(t, got.Bindings["/1#body"].Message)
				assert.Equal(t, "ok", got.Bindings["/0.1#rows"].Status, "siblings keep working")
			},
		},
		{
			// The carry-forward from Task 3: SubstituteParams can't reject an
			// undeclared parameter (it only sees one binding's template), so
			// this fan-out caller — the only place that sees the WHOLE
			// declaration's uicomponents.ParamNames — must reject it instead of
			// letting it pass through unremarked.
			name:       "an undeclared parameter rejects the whole request, never silently drops it",
			subject:    bindingsSubject,
			origin:     bindingsOrigin,
			interactOK: true,
			body:       `{"params":{"nonsense":"x"}}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "an unknown binding source errors on that binding only",
			subject:    bindingsSubject,
			origin:     bindingsOrigin,
			interactOK: true,
			body:       `{"params":{"span":"30d"}}`,
			wantStatus: http.StatusOK,
			check: func(t *testing.T, got bindingsResponseBody) {
				assert.Equal(t, "error", got.Bindings["/2#text"].Status)
				assert.Equal(t, "ok", got.Bindings["/0.1#rows"].Status, "siblings keep working")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &bindingsFakeDeps{
				interactOK:  tc.interactOK,
				interactErr: tc.interactErr,
				k8s:         newBindingsFakeK8s(t, bindingsBaseObjs()...),
				origin:      bindingsOrigin,
			}
			rec := postBindings(t, d, tc.subject, tc.origin, tc.body)
			require.Equal(t, tc.wantStatus, rec.Code, "body: %s", rec.Body.String())
			if tc.check != nil {
				var got bindingsResponseBody
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
				tc.check(t, got)
			}
		})
	}
}

// TestBindingsHandlerIgnoresClientSuppliedRefAndSource is the mutation-check
// regression guard for invariant 1: a request body naming its own ref/
// source/toolName must never be honored. bindingsRequestBody has no such
// field, so these extra keys are simply not read — but if a future change
// added one and wired it in (reading a client-chosen ref instead of the
// declaration's "list_rows"), fakeToolResolver's default branch would
// return an error for the fabricated ref and this assertion would fail.
func TestBindingsHandlerIgnoresClientSuppliedRefAndSource(t *testing.T) {
	registerFakeToolResolver(t)

	d := &bindingsFakeDeps{interactOK: true, k8s: newBindingsFakeK8s(t, bindingsBaseObjs()...), origin: bindingsOrigin}
	rec := postBindings(t, d, bindingsSubject, bindingsOrigin,
		`{"params":{"span":"30d"},"ref":"attacker_ref","source":"attacker_source","toolName":"attacker_tool"}`)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var got bindingsResponseBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "ok", got.Bindings["/0.1#rows"].Status,
		"the declaration's own ref (list_rows) must be used, never anything from the request body")
	assert.JSONEq(t, `[{"a":1}]`, string(got.Bindings["/0.1#rows"].Value))
}

// TestBindingsHandlerRejectsAnUnvalidatedAgentUI proves the same door the
// view resolution closes (ViewFor, viewmodel.go) closes here too: an AgentUI
// whose Valid condition is not True must 422, because this route is reachable
// directly and must never resolve bindings out of a document the reconciler
// has not validated.
func TestBindingsHandlerRejectsAnUnvalidatedAgentUI(t *testing.T) {
	registerFakeToolResolver(t)

	objs := []client.Object{bindingsFixtureSession(), bindingsFixtureClass(), bindingsFixtureUI(false)}
	d := &bindingsFakeDeps{interactOK: true, k8s: newBindingsFakeK8s(t, objs...), origin: bindingsOrigin}
	rec := postBindings(t, d, bindingsSubject, bindingsOrigin, `{"params":{"span":"30d"}}`)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, "body: %s", rec.Body.String())
}

// TestBindingsHandlerTooManyBindingsRefusedNotTruncated proves the fan-out
// bound (maxBindingsPerRequest) refuses the whole request rather than
// silently rendering only the first 64 bound props with no signal that the
// rest were dropped.
func TestBindingsHandlerTooManyBindingsRefusedNotTruncated(t *testing.T) {
	registerFakeToolResolver(t)

	ui := bindingsFixtureUI(true)
	ui.Spec.Slots = []spiceboxv1alpha1.AgentUISlot{
		{Name: "many", Default: &apiextensionsv1.JSON{Raw: manyBindingsSlotJSON(t, maxBindingsPerRequest+1)}},
	}
	objs := []client.Object{bindingsFixtureSession(), bindingsFixtureClass(), ui}
	d := &bindingsFakeDeps{interactOK: true, k8s: newBindingsFakeK8s(t, objs...), origin: bindingsOrigin}
	rec := postBindings(t, d, bindingsSubject, bindingsOrigin, `{"params":{}}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
}

// manyBindingsSlotJSON builds a slot with n children, each a bound
// ap:badge.text — enough to exceed maxBindingsPerRequest without hand-writing
// each node.
func manyBindingsSlotJSON(t *testing.T, n int) []byte {
	t.Helper()
	type node struct {
		Component string                    `json:"component"`
		Bindings  map[string]map[string]any `json:"bindings"`
	}
	children := make([]node, n)
	for i := range children {
		children[i] = node{Component: "ap:badge", Bindings: map[string]map[string]any{
			"text": {"source": "tool", "ref": "list_rows"},
		}}
	}
	wrapper := struct {
		Component string `json:"component"`
		Children  []node `json:"children"`
	}{Component: "ap:stack", Children: children}
	b, err := json.Marshal(wrapper)
	require.NoError(t, err)
	return b
}

// TestBindingsHandlerNeverLeaksInternalIdentifiers asserts every response
// body across a spread of error-triggering requests carries no internal
// operational vocabulary — a CRD kind, a kubectl-shaped path, a SpiceDB
// relation, or a raw client-go error string.
func TestBindingsHandlerNeverLeaksInternalIdentifiers(t *testing.T) {
	registerFakeToolResolver(t)

	forbidden := []string{"AgentUI", "AgentSession", "kubectl", "ap.session.", "mcpserver/", "spicedb", "memory.Scope"}

	cases := []struct {
		name string
		d    *bindingsFakeDeps
		body string
	}{
		{
			name: "interact errors",
			d: &bindingsFakeDeps{interactErr: errors.New("spicedb: dial tcp 10.0.0.1:443: connect: connection refused"),
				k8s: newBindingsFakeK8s(t, bindingsBaseObjs()...), origin: bindingsOrigin},
			body: `{"params":{}}`,
		},
		{
			name: "session not found",
			d: &bindingsFakeDeps{interactOK: true,
				k8s: newBindingsFakeK8s(t, bindingsFixtureClass(), bindingsFixtureUI(true)), origin: bindingsOrigin},
			body: `{"params":{}}`,
		},
		{
			name: "AgentUI not yet valid",
			d: &bindingsFakeDeps{interactOK: true,
				k8s: newBindingsFakeK8s(t, bindingsFixtureSession(), bindingsFixtureClass(), bindingsFixtureUI(false)), origin: bindingsOrigin},
			body: `{"params":{}}`,
		},
		{
			name: "undeclared parameter",
			d: &bindingsFakeDeps{interactOK: true,
				k8s: newBindingsFakeK8s(t, bindingsBaseObjs()...), origin: bindingsOrigin},
			body: `{"params":{"nonsense":"x"}}`,
		},
		{
			name: "malformed body",
			d: &bindingsFakeDeps{interactOK: true,
				k8s: newBindingsFakeK8s(t, bindingsBaseObjs()...), origin: bindingsOrigin},
			body: `{not json`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postBindings(t, tc.d, bindingsSubject, bindingsOrigin, tc.body)
			body := rec.Body.String()
			for _, f := range forbidden {
				assert.NotContains(t, body, f, "case %q: response leaked an internal identifier", tc.name)
			}
		})
	}
}

// TestBindingsHandlerAcceptsAnExpandedParameterKey pins step 6's filter to
// uicomponents.ParamKeys, at the HTTP layer, against a declaration that
// actually contains a compound-key control. The browser's parameter map is
// always keyed by the expanded runtime key, and one unknown key rejects the
// WHOLE request — so filtering against ParamNames would 400 every bindings
// request from a page carrying a date range.
//
// TestFilterParams below cannot catch this: it hand-supplies the already-
// expanded `declared` list, so it exercises the helper and never the
// ParamKeys(decl) call that produced that list.
func TestBindingsHandlerAcceptsAnExpandedParameterKey(t *testing.T) {
	d := &bindingsFakeDeps{interactOK: true, k8s: newBindingsFakeK8s(t, bindingsBaseObjs()...), origin: bindingsOrigin}
	rec := postBindings(t, d, bindingsSubject, bindingsOrigin,
		`{"params":{"span":"7d","window.from":"2026-01-01","window.to":"2026-01-31"}}`)

	require.Equal(t, http.StatusOK, rec.Code,
		"an expanded parameter key is a legal key; rejecting it breaks every binding on a page with a date range: %s", rec.Body.String())
}

// TestFilterParams unit-tests the exact-key matching rule (bindingsHandler
// step 6) independent of the HTTP layer. `declared` is what
// uicomponents.ParamKeys returns — already expanded through each control's
// registered ParamValues — so this function does exact membership and never
// re-derives the compound-name convention itself.
func TestFilterParams(t *testing.T) {
	declared := []string{"span", "window.from", "window.to"}
	cases := []struct {
		name        string
		supplied    map[string]string
		wantKept    map[string]string
		wantUnknown []string
	}{
		{
			name:     "exact match kept",
			supplied: map[string]string{"span": "30d"},
			wantKept: map[string]string{"span": "30d"},
		},
		{
			name:     "an ap:daterange's expanded compound keys kept",
			supplied: map[string]string{"window.from": "2026-01-01", "window.to": "2026-02-01"},
			wantKept: map[string]string{"window.from": "2026-01-01", "window.to": "2026-02-01"},
		},
		{
			// The prefix rule this replaced accepted anything under a declared
			// name. Only the keys a control actually drives are legal.
			name:        "an invented key under a declared compound name is not a match",
			supplied:    map[string]string{"window.midnight": "x"},
			wantKept:    map[string]string{},
			wantUnknown: []string{"window.midnight"},
		},
		{
			name:        "a compound parameter's bare declared name is not itself a key",
			supplied:    map[string]string{"window": "2026-01-01"},
			wantKept:    map[string]string{},
			wantUnknown: []string{"window"},
		},
		{
			name:        "unrelated key rejected alongside a valid one",
			supplied:    map[string]string{"span": "30d", "nonsense": "x"},
			wantKept:    map[string]string{"span": "30d"},
			wantUnknown: []string{"nonsense"},
		},
		{
			name:        "prefix without the separating dot is not a match",
			supplied:    map[string]string{"windowish": "x"},
			wantKept:    map[string]string{},
			wantUnknown: []string{"windowish"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kept, unknown := filterParams(tc.supplied, declared)
			assert.Equal(t, tc.wantKept, kept)
			assert.ElementsMatch(t, tc.wantUnknown, unknown)
		})
	}
}

// --- J1: the MATCH gate — the selector applied on the resolution path ------

// TestBindingsHandlerExtractsThroughTheDeclaredSelector is J1's SPANNING
// test: it drives the REAL bindingsHandler (via postBindings, never
// uiselect.Apply directly) over a declaration whose ap:table.rows binding
// carries a selector, and asserts the EXTRACTED value — not merely that some
// value came back. A handler that ignored Select entirely would also return
// 200/"ok" with a non-empty value (the whole envelope), so asserting the
// exact extracted bytes is what makes this catch a selector nobody reads.
func TestBindingsHandlerExtractsThroughTheDeclaredSelector(t *testing.T) {
	registerFakeToolResolver(t)

	d := &bindingsFakeDeps{interactOK: true,
		k8s:    newBindingsFakeK8s(t, selectorFixtureObjs(t, "envelope_rows", "results[].properties")...),
		origin: bindingsOrigin}
	rec := postBindings(t, d, bindingsSubject, bindingsOrigin, `{"params":{}}`)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var got bindingsResponseBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Contains(t, got.Bindings, "/0#rows")
	assert.Equal(t, "ok", got.Bindings["/0#rows"].Status)
	assert.JSONEq(t, `[{"name":"Acme"},{"name":"Globex"}]`, string(got.Bindings["/0#rows"].Value),
		"the response must carry the value AFTER selection, not the raw envelope resolveOneBinding got back")
}

// TestBindingsHandlerReportsASelectorThatDoesNotMatch proves the two-surface
// rule for a MissError reaching resolveOneBinding: fixed, leak-free browser
// copy, and a structured operator log carrying the failing segment and the
// upstream keys that WERE present. The request itself still succeeds — a
// broken selector degrades one binding, never the page.
func TestBindingsHandlerReportsASelectorThatDoesNotMatch(t *testing.T) {
	registerFakeToolResolver(t)
	var sink strings.Builder

	d := &bindingsFakeDeps{interactOK: true,
		k8s:     newBindingsFakeK8s(t, selectorFixtureObjs(t, "envelope_rows", "items[].properties")...),
		origin:  bindingsOrigin,
		logSink: &sink}
	rec := postBindings(t, d, bindingsSubject, bindingsOrigin, `{"params":{}}`)
	require.Equal(t, http.StatusOK, rec.Code, "a failed binding must never fail the request: body: %s", rec.Body.String())

	var got bindingsResponseBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Contains(t, got.Bindings, "/0#rows")
	assert.Equal(t, "error", got.Bindings["/0#rows"].Status)
	assert.Equal(t, selectorMissMessage, got.Bindings["/0#rows"].Message)

	// No selector, no upstream key, no upstream value in browser copy.
	msg := got.Bindings["/0#rows"].Message
	assert.NotContains(t, msg, "items")
	assert.NotContains(t, msg, "results")
	assert.NotContains(t, msg, "Acme")

	// The two-surface rule: the diagnostic the browser copy withholds MUST
	// exist somewhere, or an operator has a broken page with no way to find
	// out why. found is keys-only ("results", "total"), never the values.
	logged := sink.String()
	assert.Contains(t, logged, `items[].properties`, "operator log must carry the failing selector")
	assert.Contains(t, logged, "results", "operator log must carry the keys that WERE present")
	assert.Contains(t, logged, "total", "operator log must carry the keys that WERE present")
}

// TestBindingsHandlerRefusesAnUnparseableSelector is the belt to
// uicomponents.Validate's syntax-gate suspenders: a selector that could not
// have passed author-time validation (a CR applied against a newer platform,
// a reconciler that has not caught up) must still be refused here — never a
// panic, never silently ignored. The call counter is the point: asserting
// only the message would also pass an implementation that resolves FIRST and
// parses after, which hammers the upstream on every render of a page with
// one broken selector — see task-3-brief's ordering rule.
func TestBindingsHandlerRefusesAnUnparseableSelector(t *testing.T) {
	r := registerFakeToolResolver(t)

	d := &bindingsFakeDeps{interactOK: true,
		k8s:    newBindingsFakeK8s(t, selectorFixtureObjs(t, "envelope_rows", "a[0]")...),
		origin: bindingsOrigin}
	rec := postBindings(t, d, bindingsSubject, bindingsOrigin, `{"params":{}}`)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var got bindingsResponseBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Contains(t, got.Bindings, "/0#rows")
	assert.Equal(t, "error", got.Bindings["/0#rows"].Status)
	assert.Equal(t, selectorInvalidMessage, got.Bindings["/0#rows"].Message)
	assert.Equal(t, int32(0), r.calls.Load(), "an unparseable selector must be refused BEFORE the resolver is ever called")
}

// TestBindingsHandlerLeavesAnUnselectedBindingByteIdentical pins the
// un-round-tripped guarantee (uiselect.Selector.Apply's own doc: the empty
// selector returns the input UNCHANGED, not a re-encoding) at the handler
// level, using the pre-existing list_rows binding (rootSlotJSON) which
// carries no `select` at all — exactly what every binding written before
// selectors existed looks like. assert.Equal on the raw bytes, not
// assert.JSONEq, because the point is byte identity, not merely equivalent
// JSON (a re-marshal could reorder object keys and still pass JSONEq).
func TestBindingsHandlerLeavesAnUnselectedBindingByteIdentical(t *testing.T) {
	registerFakeToolResolver(t)

	d := &bindingsFakeDeps{interactOK: true, k8s: newBindingsFakeK8s(t, bindingsBaseObjs()...), origin: bindingsOrigin}
	rec := postBindings(t, d, bindingsSubject, bindingsOrigin, `{"params":{"span":"30d"}}`)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var got bindingsResponseBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Contains(t, got.Bindings, "/0.1#rows")
	assert.Equal(t, "ok", got.Bindings["/0.1#rows"].Status)
	assert.Equal(t, `[{"a":1}]`, string(got.Bindings["/0.1#rows"].Value))
}

// TestBindingsHandlerAcceptsASelectorThatMatchesAnEmptyArray is the
// miss/empty distinction restated at THIS layer: a matched path holding an
// empty array is an ordinary, successful "zero rows" outcome — the layer
// that decides whether the browser sees a card or a table must not confuse
// it with a selector that failed to match anything.
func TestBindingsHandlerAcceptsASelectorThatMatchesAnEmptyArray(t *testing.T) {
	registerFakeToolResolver(t)

	d := &bindingsFakeDeps{interactOK: true,
		k8s:    newBindingsFakeK8s(t, selectorFixtureObjs(t, "empty_envelope", "results[]")...),
		origin: bindingsOrigin}
	rec := postBindings(t, d, bindingsSubject, bindingsOrigin, `{"params":{}}`)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var got bindingsResponseBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Contains(t, got.Bindings, "/0#rows")
	assert.Equal(t, "ok", got.Bindings["/0#rows"].Status)
	assert.JSONEq(t, `[]`, string(got.Bindings["/0#rows"].Value))
}

// --- the CEILING CORRECTION: the extracted value needs its own meter -------
//
// Plan 8 originally reasoned the ingress ceiling could stay on the RAW
// upstream response alone, because "extraction can only shrink a document".
// Task 1 disproved that by test: encoding/json's default marshaler escapes
// a single '<' byte to a six-byte unicode-escape sequence on re-marshal
// (HTML-embedding safety) with no stdlib opt-out, so a SELECTED value can
// re-marshal larger than the compact document it was extracted from. See
// .superpowers/sdd/2026-08-08-agent-ui-binding-selectors/CEILING-CORRECTION.md.
//
// The fixture below is sized so the property is unmistakable, not merely
// present: the raw envelope (~1,000,008 bytes) sits comfortably UNDER
// toolguard.DefaultUIIngressBytes (4 MiB), while the value the selector
// extracts (a bare JSON string of 1,000,000 '<' characters, each escaped to
// six bytes on re-marshal: 6,000,002 bytes total) sits comfortably OVER it —
// so a ceiling that inspected only the raw response would let this straight
// through, and only a SECOND meter on the extracted value catches it.
func selectorGrowthFixtureObjs(t *testing.T, n int) []client.Object {
	t.Helper()
	raw := rawGrowthEnvelope(n)
	require.Less(t, int64(len(raw)), toolguard.DefaultUIIngressBytes, "fixture bug: the raw envelope must itself be under the ceiling")

	node := struct {
		Component string                    `json:"component"`
		Bindings  map[string]map[string]any `json:"bindings"`
	}{
		Component: "ap:markdown",
		Bindings: map[string]map[string]any{
			"body": {"source": "tool", "ref": "large_field", "args": map[string]any{}, "select": "a"},
		},
	}
	nodeRaw, err := json.Marshal(node)
	require.NoError(t, err)

	ui := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: bindingsUIName, Namespace: bindingsNS},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Slots: []spiceboxv1alpha1.AgentUISlot{{Name: "sel", Default: &apiextensionsv1.JSON{Raw: nodeRaw}}},
		},
		Status: spiceboxv1alpha1.AgentUIStatus{
			Conditions: []metav1.Condition{{Type: spiceboxv1alpha1.AgentUIConditionValid, Status: metav1.ConditionTrue, Reason: "Test"}},
		},
	}
	return []client.Object{bindingsFixtureSession(), bindingsFixtureClass(), ui}
}

// rawGrowthEnvelope builds `{"a":"<<<...<"}` (n '<' characters) by direct
// byte concatenation, NEVER json.Marshal — a Go json.Marshal call ALSO
// HTML-escapes by default, which would defeat the fixture's whole point of
// standing in for an upstream HTTP response's literal, un-escaped bytes. A
// bare '<' needs no escaping to be valid JSON inside a string, which is
// exactly what makes it possible for the raw form to be small while
// uiselect.Selector.Apply's re-marshal of the SAME character is not.
func rawGrowthEnvelope(n int) []byte {
	return []byte(`{"a":"` + strings.Repeat("<", n) + `"}`)
}

// largeFieldToolResolver stands in for "tool" only for the growth fixture: it
// hands back a raw envelope UNDER the ceiling, sized precisely by n.
type largeFieldToolResolver struct{ n int }

func (largeFieldToolResolver) Source() string { return "tool" }

func (r largeFieldToolResolver) Resolve(_ context.Context, _ uibindings.Deps, req uibindings.Request) (uibindings.Result, error) {
	if req.Ref != "large_field" {
		return uibindings.Result{}, errors.New("this view's data could not be loaded")
	}
	return uibindings.Result{Value: rawGrowthEnvelope(r.n)}, nil
}

// TestBindingsHandlerWithholdsAnExtractedValueOverTheUIIngressCeiling is the
// ceiling correction's own test: it fails if the extracted-value meter does
// not exist (only the mutation run proves that — see the task report), and
// it fails here and now if a passing implementation truncated instead of
// withholding, because Value must be EMPTY on the error result, not a
// partial document (a truncated JSON document is not parseable, which would
// turn a size failure into a confusing parse failure on the browser side).
func TestBindingsHandlerWithholdsAnExtractedValueOverTheUIIngressCeiling(t *testing.T) {
	registry.Reset()
	t.Cleanup(registry.Reset)
	registry.Register(largeFieldToolResolver{n: 1_000_000})

	d := &bindingsFakeDeps{interactOK: true,
		k8s:    newBindingsFakeK8s(t, selectorGrowthFixtureObjs(t, 1_000_000)...),
		origin: bindingsOrigin}
	rec := postBindings(t, d, bindingsSubject, bindingsOrigin, `{"params":{}}`)
	require.Equal(t, http.StatusOK, rec.Code, "an oversized binding must never fail the whole request: body: %s", rec.Body.String())

	var got bindingsResponseBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Contains(t, got.Bindings, "/0#body")
	assert.Equal(t, "error", got.Bindings["/0#body"].Status)
	assert.Equal(t, selectorTooLargeMessage, got.Bindings["/0#body"].Message)
	assert.Empty(t, got.Bindings["/0#body"].Value, "over-limit must be WITHHELD, not truncated")
}
