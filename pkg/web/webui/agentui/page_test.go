package agentui_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/agentui"
)

const (
	pageTrustedHost = "trusted.example"
	pageSandboxHost = "sandbox.example"
	// pageSessionName is the URL's {name} segment for every case below: an
	// AgentSession name, exactly like sessionview's /session-view/{ns}/{name}
	// — CheckInteract is hardwired to the "agentsession" SpiceDB resource type
	// (pkg/authz/spicedb/client.go's CheckInteract), so a top-level CheckInteract
	// call at this page's door is only ever meaningful against a real session
	// name, never an AgentClass name.
	pageSessionName = "sess-live"
	pageAgentUIName = "console-ui"
)

// newFakeK8s builds a controller-runtime fake client carrying the v1alpha1
// types this page reads (AgentSession, AgentClass, AgentUI). getErr, when
// non-nil, is consulted before every Get: returning a non-nil error from it
// simulates a control-plane failure that is NOT a NotFound (an API-server
// blip, an RBAC change), which is the only way to reach the page's 500 doors —
// omitting an object yields NotFound instead.
func newFakeK8s(t *testing.T, getErr func(obj client.Object) error, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
	b := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...)
	if getErr != nil {
		b = b.WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if err := getErr(obj); err != nil {
					return err
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
	}
	return b.Build()
}

// getFailsFor returns a getErr that fails only for objects of the same
// concrete type as want — so a row can break exactly one of the page's three
// Gets and leave the others healthy.
func getFailsFor(want client.Object) func(client.Object) error {
	return func(obj client.Object) error {
		if reflect.TypeOf(obj) != reflect.TypeOf(want) {
			return nil
		}
		return errors.New("etcdserver: request timed out")
	}
}

// pageFixtureSession is a live (Running) AgentSession named pageSessionName
// for testAgentClass — the session the top-level CheckInteract gate checks,
// and the one ResolveSession's ladder is keyed to (via .Spec.Class for the
// AgentClass lookup, and the object itself for the ladder branch).
func pageFixtureSession() *spiceboxv1alpha1.AgentSession {
	return pageFixtureSessionPhase(spiceboxv1alpha1.AgentSessionPhaseRunning, time.Minute)
}

// pageFixtureSessionPhase builds the NAMED session (pageSessionName) in a
// given phase/creation-offset — reuses session_test.go's session() helper so
// namespace/class stay in lockstep with pageFixtureAgentClass.
func pageFixtureSessionPhase(phase string, createdAgo time.Duration) *spiceboxv1alpha1.AgentSession {
	s := session(pageSessionName, phase, createdAgo)
	return &s
}

// pageFixtureDecoySession builds an UNNAMED (not the URL's {name}) session
// for the same class/namespace — used to prove the ladder never substitutes
// a different session for the one the URL names, regardless of how recently
// it was created (see TestViewForDoors_FailClosedAndLogTheCause's
// newer-session row).
func pageFixtureDecoySession(name string, createdAgo time.Duration) *spiceboxv1alpha1.AgentSession {
	s := session(name, spiceboxv1alpha1.AgentSessionPhaseRunning, createdAgo)
	return &s
}

// pageFixtureAgentClass builds the AgentClass every case Gets by name
// (testAgentClass, from the fixture session's .Spec.Class). grant is nil to
// exercise "no AgentUI on the class".
func pageFixtureAgentClass(grant *spiceboxv1alpha1.AgentClassUIGrant) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: testAgentClass, Namespace: testNamespace},
		Spec:       spiceboxv1alpha1.AgentClassSpec{AgentUI: grant},
	}
}

// pageFixtureAgentUI builds the referenced AgentUI CR with one slot (root)
// carrying a fabricated Tier-0 declaration, and a Valid condition set per
// valid/message.
func pageFixtureAgentUI(valid bool, message string) *spiceboxv1alpha1.AgentUI {
	status := metav1.ConditionFalse
	if valid {
		status = metav1.ConditionTrue
	}
	return &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Name: pageAgentUIName, Namespace: testNamespace},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Slots: []spiceboxv1alpha1.AgentUISlot{
				{Name: "root", Default: &apiextensionsv1.JSON{Raw: []byte(`{"component":"ap:stack"}`)}},
			},
		},
		Status: spiceboxv1alpha1.AgentUIStatus{
			Conditions: []metav1.Condition{
				{Type: spiceboxv1alpha1.AgentUIConditionValid, Status: status, Reason: "Test", Message: message},
			},
		},
	}
}

// pageFixtureAgentUICondition builds the referenced AgentUI with an explicit
// Valid condition status/reason/message — the Unknown rung the reconciler sets
// when its own AgentClass read fails, whose message wraps a raw client-go
// error (see pkg/controllers/agentui's ReasonAgentUIGrantUnresolved branch).
func pageFixtureAgentUICondition(status metav1.ConditionStatus, reason, message string) *spiceboxv1alpha1.AgentUI {
	aui := pageFixtureAgentUI(true, "")
	aui.Status.Conditions = []metav1.Condition{
		{Type: spiceboxv1alpha1.AgentUIConditionValid, Status: status, Reason: reason, Message: message},
	}
	return aui
}

// pageFixtureAgentUINoConditions builds the referenced AgentUI with an empty
// status — the state every AgentUI is in between creation and its first
// reconcile, i.e. what a fresh install passes through.
func pageFixtureAgentUINoConditions() *spiceboxv1alpha1.AgentUI {
	aui := pageFixtureAgentUI(true, "")
	aui.Status.Conditions = nil
	return aui
}

// newPageServer wires the agentui WebUI into a real webui.Server whose
// authenticate func NEVER authenticates — no cookie, ever — mirroring
// sessionview_test.go's newServer helper.
//
// Refusing to authenticate is the point: it is what lets the test below prove
// the address answers a redirect rather than a login flow.
func newPageServer(t *testing.T, d agentui.Deps) *webui.Server {
	t.Helper()
	authenticate := func(r *http.Request) (string, bool) { return "", false }
	s, err := webui.NewServer(authenticate, nil,
		func() string { return pageTrustedHost }, func() string { return pageSandboxHost },
		nil, d, []webui.WebUI{agentui.New()})
	require.NoError(t, err)
	return s
}

func pageReq(host, path string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
	r.Host = host
	return r
}

// TestAddressRedirectsThroughTheRealServerWithoutACookie is the framework half
// of the redirect's contract, which a direct handler call cannot show: mounted
// at AuthNone, the address answers a cookie-less GET with the redirect itself,
// not with a login flow.
//
// That is what makes the address usable everywhere it is handed out — a Slack
// button, a link pasted to a colleague. The login happens at the TARGET, which
// is AuthLoginIfNecessary, so the viewer still signs in before anything about
// the session is resolved; they simply do it with a `next` that returns them
// here instead of bouncing off a 401.
func TestAddressRedirectsThroughTheRealServerWithoutACookie(t *testing.T) {
	d := fakeAgentUI()
	d.k8s = newFakeK8s(t, nil, pageFixtureSession(),
		pageFixtureAgentClass(&spiceboxv1alpha1.AgentClassUIGrant{Ref: pageAgentUIName}),
		pageFixtureAgentUI(true, ""))

	rec := httptest.NewRecorder()
	newPageServer(t, d).ServeHTTP(rec, pageReq(pageTrustedHost, "/agent-ui/"+testNamespace+"/"+pageSessionName))

	require.Equal(t, http.StatusFound, rec.Code,
		"a cookie-less GET must be redirected, not answered with 401 or a login page")
	assert.Equal(t, "/sessions?session="+testNamespace+"%2F"+pageSessionName, rec.Header().Get("Location"))
}

// TestViewForDoors_FailClosedAndLogTheCause table-drives every fail-closed
// door the session -> class -> UI ladder crosses, through the exported entry
// point the session shell itself calls.
//
// The rows are the ones that used to run through this package's own page:
// a session that vanished mid-flight, each Get that can fail for a reason that
// is NOT a NotFound, a dangling AgentUI reference, an AgentUI the controller
// has not reconciled yet, a Valid=Unknown whose message wraps a raw client-go
// error, a Valid=False verdict, a terminal session, and the two "named session
// is never substituted" regressions. The page is gone; the doors are not — the
// three /agent-ui/{ns}/{name}/... API routes and the shell's selected view both
// walk them.
//
// Two properties are asserted on every failing row, because they are the ones
// that regress silently:
//
//   - the user-facing Message carries NO internal operational detail (an
//     API-server address, a resource path, a client-go error string), and
//   - the cause that was withheld from the browser DOES reach the log.
//
// The authorization door is deliberately absent: ViewFor documents that its
// caller has already confirmed interact, and pkg/web/webui/sessions owns that gate
// and tests it.
func TestViewForDoors_FailClosedAndLogTheCause(t *testing.T) {
	tests := []struct {
		name string
		objs []client.Object
		// getErr, when set, fails one object kind's Get — the only way to reach
		// a 500 door, since an absent object yields NotFound.
		getErr func(client.Object) error
		// wantStatus is the *webui.PageError status; 0 means "no door error".
		wantStatus int
		// wantAvail is only consulted when wantStatus is 0.
		wantAvail       agentui.Availability
		wantMessageHas  []string
		wantMessageOmit []string
		wantLogHas      []string
		// check runs on a successful resolution.
		check func(t *testing.T, props agentui.ViewProps)
	}{
		{
			// Defense in depth behind the shell's interact gate: the session was
			// deleted between the authorization check and the read.
			name: "session deleted mid-flight -> 404, naming no Kubernetes vocabulary",
			objs: []client.Object{pageFixtureAgentClass(&spiceboxv1alpha1.AgentClassUIGrant{Ref: pageAgentUIName}),
				pageFixtureAgentUI(true, "")},
			wantStatus:      http.StatusNotFound,
			wantMessageHas:  []string{"This session could not be found."},
			wantMessageOmit: []string{"agentsessions", "apis/"},
		},
		{
			name: "session read fails (not NotFound) -> 500, generic copy, cause logged",
			objs: []client.Object{pageFixtureSession(),
				pageFixtureAgentClass(&spiceboxv1alpha1.AgentClassUIGrant{Ref: pageAgentUIName}),
				pageFixtureAgentUI(true, "")},
			getErr:          getFailsFor(&spiceboxv1alpha1.AgentSession{}),
			wantStatus:      http.StatusInternalServerError,
			wantMessageHas:  []string{"Could not load this session."},
			wantMessageOmit: []string{"etcdserver"},
			wantLogHas:      []string{"etcdserver", pageSessionName},
		},
		{
			// A dangling sess.Spec.Class: the AgentClass the session names is
			// gone, so nothing can know which AgentUI to resolve.
			name: "AgentClass read fails -> 500, generic copy, cause logged",
			objs: []client.Object{pageFixtureSession(),
				pageFixtureAgentClass(&spiceboxv1alpha1.AgentClassUIGrant{Ref: pageAgentUIName}),
				pageFixtureAgentUI(true, "")},
			getErr:          getFailsFor(&spiceboxv1alpha1.AgentClass{}),
			wantStatus:      http.StatusInternalServerError,
			wantMessageHas:  []string{"Could not load this agent's configuration."},
			wantMessageOmit: []string{"etcdserver"},
			wantLogHas:      []string{"etcdserver", testAgentClass},
		},
		{
			// Likely in practice: a dangling spec.agentUI.ref right after an
			// agent install, or an AgentUI deleted while its AgentClass still
			// references it.
			name: "referenced AgentUI does not exist -> 404 UI unavailable",
			objs: []client.Object{pageFixtureSession(),
				pageFixtureAgentClass(&spiceboxv1alpha1.AgentClassUIGrant{Ref: pageAgentUIName})},
			wantStatus:      http.StatusNotFound,
			wantMessageHas:  []string{"This agent's UI isn't available right now."},
			wantMessageOmit: []string{"agentuis", pageAgentUIName},
		},
		{
			// Every fresh install passes through this: the AgentUI CR exists but
			// its controller has not reconciled it yet. Transient and nobody's
			// fault, so it is a 503 with retry-shaped copy, never a 422 verdict
			// on the bundle.
			name: "AgentUI not reconciled yet (no conditions) -> 503, generic retry copy",
			objs: []client.Object{pageFixtureSession(),
				pageFixtureAgentClass(&spiceboxv1alpha1.AgentClassUIGrant{Ref: pageAgentUIName}),
				pageFixtureAgentUINoConditions()},
			wantStatus:     http.StatusServiceUnavailable,
			wantMessageHas: []string{"Try again in a moment."},
			wantLogHas:     []string{"undetermined", pageAgentUIName},
		},
		{
			// I2: the reconciler's Valid=Unknown message wraps a RAW client-go
			// error. Surfacing it verbatim (as a 422 "UI not ready") both blamed
			// the bundle for a control-plane hiccup and put an API-server address
			// in front of any authenticated viewer.
			name: "AgentUI Valid=Unknown -> 503 with no client-go detail in the message, cause logged",
			objs: []client.Object{pageFixtureSession(),
				pageFixtureAgentClass(&spiceboxv1alpha1.AgentClassUIGrant{Ref: pageAgentUIName}),
				pageFixtureAgentUICondition(metav1.ConditionUnknown, spiceboxv1alpha1.ReasonAgentUIGrantUnresolved,
					`could not resolve the AgentClass grant: list agentclasses.agentprimitives.authzed.com in namespace `+
						testNamespace+`: Get "https://10.96.0.1:443/apis/agentprimitives.authzed.com/v1alpha1/agentclasses": `+
						`dial tcp 10.96.0.1:443: connect: connection refused`)},
			wantStatus:      http.StatusServiceUnavailable,
			wantMessageHas:  []string{"Try again in a moment."},
			wantMessageOmit: []string{"10.96.0.1", "agentclasses", "dial tcp", "apis/", "AgentClass grant"},
			wantLogHas:      []string{"10.96.0.1", spiceboxv1alpha1.ReasonAgentUIGrantUnresolved},
		},
		{
			// Valid=False IS a verdict on the bundle, and its message is the
			// author's own vocabulary — the one door whose message is worth
			// showing, because the person who can act on it wrote the UI.
			name: "AgentUI Valid=False -> 422 carrying the author's own message",
			objs: []client.Object{pageFixtureSession(),
				pageFixtureAgentClass(&spiceboxv1alpha1.AgentClassUIGrant{Ref: pageAgentUIName}),
				pageFixtureAgentUI(false, "slot root: unknown component ap:bogus")},
			wantStatus:     http.StatusUnprocessableEntity,
			wantMessageHas: []string{"slot root: unknown component ap:bogus"},
		},
		{
			// C2: a UI is always session-scoped, so a terminal named session must
			// never resolve to success with an empty session. Starting a
			// replacement is an explicit POST, never this path's side effect.
			name: "named session is terminal (Succeeded) -> 410, never success with an empty session",
			objs: []client.Object{pageFixtureSessionPhase(spiceboxv1alpha1.AgentSessionPhaseSucceeded, time.Hour),
				pageFixtureAgentClass(&spiceboxv1alpha1.AgentClassUIGrant{Ref: pageAgentUIName}),
				pageFixtureAgentUI(true, "")},
			wantStatus:     http.StatusGone,
			wantMessageHas: []string{"This session has ended."},
		},
		{
			// The one door ViewFor answers differently from resolveAgentUIDoors:
			// an agent with no declared UI is the shell's ordinary "show chat"
			// case, not a 404.
			name:      "no AgentUI on the class -> UINotDeclared, not an error",
			objs:      []client.Object{pageFixtureSession(), pageFixtureAgentClass(nil)},
			wantAvail: agentui.UINotDeclared,
		},
		{
			name: "happy path -> props carry ns/name/declaration",
			objs: []client.Object{pageFixtureSession(),
				pageFixtureAgentClass(&spiceboxv1alpha1.AgentClassUIGrant{Ref: pageAgentUIName}),
				pageFixtureAgentUI(true, "")},
			wantAvail: agentui.UIAvailable,
			check: func(t *testing.T, props agentui.ViewProps) {
				assert.Equal(t, testNamespace, props.Ns)
				assert.Equal(t, pageSessionName, props.Name)
				// The full {"view":{...}} envelope, not a bare node — I3: a
				// reviewer mutated the marshal into the raw tree (dropping the
				// envelope) and the suite stayed green without this.
				assert.Contains(t, string(props.Declaration), `{"view":{"component":"ap:stack"`)
				assert.Contains(t, string(props.Declaration), `"children":[{"component":"ap:stack"}]`)
			},
		},
		{
			name: "named session is Idle -> still resolves (the branch is the shell's to disclose)",
			objs: []client.Object{pageFixtureSessionPhase(spiceboxv1alpha1.AgentSessionPhaseIdle, time.Hour),
				pageFixtureAgentClass(&spiceboxv1alpha1.AgentClassUIGrant{Ref: pageAgentUIName}),
				pageFixtureAgentUI(true, "")},
			wantAvail: agentui.UIAvailable,
			check: func(t *testing.T, props agentui.ViewProps) {
				assert.Equal(t, pageSessionName, props.Name)
			},
		},
		{
			// C1 regression row 1: a newer session exists for the same class. The
			// NAMED session must still resolve — never be silently discarded in
			// favor of the more recent one, which is what an earlier "most recent
			// session for this class" ladder did.
			name: "a newer session for the same class does not steal the resolution",
			objs: []client.Object{pageFixtureSessionPhase(spiceboxv1alpha1.AgentSessionPhaseRunning, 2*time.Hour),
				pageFixtureDecoySession("sess-newer", time.Minute),
				pageFixtureAgentClass(&spiceboxv1alpha1.AgentClassUIGrant{Ref: pageAgentUIName}),
				pageFixtureAgentUI(true, "")},
			wantAvail: agentui.UIAvailable,
			check: func(t *testing.T, props agentui.ViewProps) {
				assert.Equal(t, pageSessionName, props.Name, "the URL's own session, never sess-newer")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var logged []string
			d := fakeAgentUI()
			d.logger = funcr.New(func(prefix, args string) {
				mu.Lock()
				defer mu.Unlock()
				logged = append(logged, prefix+" "+args)
			}, funcr.Options{})
			d.k8s = newFakeK8s(t, tc.getErr, tc.objs...)

			props, _, avail, pe := agentui.ViewFor(context.Background(), d, testNamespace, pageSessionName)

			if tc.wantStatus != 0 {
				require.NotNil(t, pe, "case %q must answer a door error", tc.name)
				assert.Equal(t, tc.wantStatus, pe.Status)
				for _, want := range tc.wantMessageHas {
					assert.Contains(t, pe.Message, want, "case %q", tc.name)
				}
				for _, unwanted := range tc.wantMessageOmit {
					assert.NotContains(t, pe.Message, unwanted,
						"case %q: a user-facing message must not carry internal operational detail", tc.name)
				}
			} else {
				require.Nil(t, pe, "case %q must resolve", tc.name)
				assert.Equal(t, tc.wantAvail, avail)
				if tc.check != nil {
					tc.check(t, props)
				}
			}

			mu.Lock()
			joined := strings.Join(logged, "\n")
			mu.Unlock()
			for _, want := range tc.wantLogHas {
				assert.Contains(t, joined, want,
					"case %q: a cause withheld from the browser must still reach the log", tc.name)
			}
		})
	}
}
