package agentui_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/web/browsersession"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/agentui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/browserstart"
)

// fakeDeps implements agentui.Deps via per-behavior func fields, mirroring
// sessionview_test.go's fakeDeps. Every field beyond checkInteract/k8s/logger
// stays at its harmless zero value (empty TrustedOrigin, nil NATSRequest/
// Memory/Artifacts/ArtifactRenderBytes) unless a bindings_test.go case
// overrides it — the GET-page rows in page_test.go never reach any of them.
type fakeDeps struct {
	checkInteract func(ctx context.Context, ns, name, subject string) (bool, error)
	k8s           client.Client
	logger        logr.Logger

	trustedOrigin       string
	natsRequest         channelevents.RequestFunc
	mem                 memory.Memory
	artSvc              *artifacts.Service
	artifactRenderBytes uibindings.ArtifactRenderBytesFunc
	nc                  *nats.Conn
	// startBrowserSession stays nil by default: every row in this file's
	// tests exercises the four unconditional routes (the redirect plus
	// bindings/actions/live), never the start route, so the collaborator's
	// zero value (route omitted) is exactly right unless a future case in
	// this file needs it.
	startBrowserSession browsersession.StartFunc
}

func (f *fakeDeps) CheckInteract(ctx context.Context, ns, name, subject string) (bool, error) {
	if f.checkInteract == nil {
		return true, nil
	}
	return f.checkInteract(ctx, ns, name, subject)
}
func (f *fakeDeps) K8s() client.Client  { return f.k8s }
func (f *fakeDeps) Logger() logr.Logger { return f.logger }

func (f *fakeDeps) TrustedOrigin() string                  { return f.trustedOrigin }
func (f *fakeDeps) NATSRequest() channelevents.RequestFunc { return f.natsRequest }
func (f *fakeDeps) Memory() memory.Memory                  { return f.mem }
func (f *fakeDeps) Artifacts() *artifacts.Service          { return f.artSvc }
func (f *fakeDeps) ArtifactRenderBytes() uibindings.ArtifactRenderBytesFunc {
	return f.artifactRenderBytes
}
func (f *fakeDeps) NATS() *nats.Conn { return f.nc }

func (f *fakeDeps) StartBrowserSession() browsersession.StartFunc { return f.startBrowserSession }

// LiveSessions is nil: no case in this file posts to the start route, which is
// the only caller that reserves a slot.
func (f *fakeDeps) LiveSessions() browserstart.LiveSessions { return nil }
func (f *fakeDeps) StartableNamespaces() []string           { return []string{testNamespace} }
func (f *fakeDeps) WorkshopNamespacesFor(context.Context, string) ([]string, error) {
	return nil, nil
}

func fakeAgentUI() *fakeDeps {
	return &fakeDeps{logger: logr.Discard(), mem: &fakeMemory{}}
}

// fakeMemory is a minimal thread-safe memory.Memory test double — the
// external-package twin of live_test.go's fakeUIActionMemory (unexported
// there, in package agentui, so not visible from here). Backs
// resolveView/uiview.Resolve's uiviewmodel.List call: every row in this
// package's tests is Tier-0-only (no stored fragment), so an always-empty
// Query is sufficient; Put exists only so the type satisfies memory.Memory.
type fakeMemory struct {
	mu      sync.Mutex
	entries map[string]memory.Entry
}

func (m *fakeMemory) Put(_ context.Context, e memory.Entry) (memory.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = map[string]memory.Entry{}
	}
	m.entries[e.ID] = e
	return e, nil
}

func (m *fakeMemory) Query(_ context.Context, q memory.Query) (memory.QueryResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]memory.Entry, 0, len(m.entries))
	for _, e := range m.entries {
		if len(q.Kinds) > 0 && !slices.Contains(q.Kinds, e.Kind) {
			continue
		}
		out = append(out, e)
	}
	return memory.QueryResult{Entries: out}, nil
}

func (m *fakeMemory) Search(context.Context, memory.SearchRequest) (memory.MergedSearchResult, error) {
	return memory.MergedSearchResult{}, nil
}
func (m *fakeMemory) SendSignal(context.Context, memory.Signal) error { return nil }

var _ memory.Memory = (*fakeMemory)(nil)

// loggerOnlyDeps implements ONLY Logger() — it deliberately does NOT satisfy
// agentui.Deps (no CheckInteract, no K8s). It stands in for a webd umbrella
// that is missing one required collaborator but still carries a logger (the
// realistic misconfiguration: internal/cmd/webd's *webdDeps identity-only umbrella
// already has Logger()-adjacent collaborators wired before the viewer-only
// ones are). Routes must still fail closed AND log loudly through it.
type loggerOnlyDeps struct {
	logger logr.Logger
}

func (d *loggerOnlyDeps) Logger() logr.Logger { return d.logger }

// TestRoutes_DepsCastFails_NoRoutes proves the fail-closed contract for the
// simplest cast failure: a nil deps value yields no routes (never a panic).
// This is also exactly how pkg/web/webui's TestRegisteredPageAppsResolveInManifest
// calls every registered WebUI's Routes(nil).
func TestRoutes_DepsCastFails_NoRoutes(t *testing.T) {
	ui := agentui.New()
	assert.Nil(t, ui.Routes(nil))
}

// TestRoutes_DepsCastFails_WrongConcreteType_NoRoutesAndLogsLoudly is the
// regression guard for the exact bug internal/cmd/webd's buildArtifactViewDeps (its
// nil-on-unconfigured-prerequisites return path) documents: a deps value
// whose cast to the plugin's own Deps interface fails must not
// just fail closed silently — it must log loudly THROUGH whatever collaborator
// the failed value can still supply, so an operator sees a diagnosable
// misconfiguration instead of a bare 404 with nothing in the logs.
//
// This test is also the mutation check called out in the task: if Routes were
// changed to build its route set unconditionally (skipping the deps.(Deps)
// cast), this test fails because loggerOnlyDeps does not implement
// agentui.Deps — proving the cast is load-bearing, not decorative.
func TestRoutes_DepsCastFails_WrongConcreteType_NoRoutesAndLogsLoudly(t *testing.T) {
	var mu sync.Mutex
	var logged []string
	capLogger := funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, args)
	}, funcr.Options{})

	d := &loggerOnlyDeps{logger: capLogger}
	ui := agentui.New()

	routes := ui.Routes(d)

	assert.Nil(t, routes, "a deps value missing required collaborators must yield no routes")

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, logged, "a cast failure that can still reach a logger must log, not stay silent")
	joined := strings.Join(logged, "\n")
	assert.Contains(t, joined, "agentui", "log must identify the failing plugin")
	assert.Contains(t, joined, "cast", "log must identify this as a deps-cast failure")
}

// TestRoutes_HappyPath_ReturnsExpectedRouteSet asserts the route shape all
// four routes require: the GET address (OriginTrusted, AuthNone, a raw
// Handler that redirects into the session shell), the POST bindings route,
// the POST actions route, and the GET live route (all three OriginTrusted,
// AuthAuthenticated, a raw Handler — none returns an HTML document).
func TestRoutes_HappyPath_ReturnsExpectedRouteSet(t *testing.T) {
	d := fakeAgentUI()
	ui := agentui.New()

	routes := ui.Routes(d)

	require.Len(t, routes, 4, "the GET address, the POST bindings route, the POST actions route, and the GET live route are all wired")

	page := routes[0]
	assert.Equal(t, webui.OriginTrusted, page.Origin)
	assert.Equal(t, "/agent-ui/{ns}/{name}", page.Pattern)
	assert.Equal(t, []string{http.MethodGet}, page.Methods)
	assert.Equal(t, webui.AuthNone, page.Auth, "the redirect discloses nothing and performs no lookup")
	assert.Nil(t, page.Page, "this address no longer serves a document; it redirects")
	assert.NotNil(t, page.Handler, "a raw http.Handler must be set")

	bindings := routes[1]
	assert.Equal(t, webui.OriginTrusted, bindings.Origin)
	assert.Equal(t, "/agent-ui/{ns}/{name}/bindings", bindings.Pattern)
	assert.Equal(t, []string{http.MethodPost}, bindings.Methods)
	assert.Equal(t, webui.AuthAuthenticated, bindings.Auth)
	assert.Nil(t, bindings.Page, "bindings returns JSON, not an HTML document")
	assert.NotNil(t, bindings.Handler, "a raw http.Handler must be set")

	actions := routes[2]
	assert.Equal(t, webui.OriginTrusted, actions.Origin)
	assert.Equal(t, "/agent-ui/{ns}/{name}/actions", actions.Pattern)
	assert.Equal(t, []string{http.MethodPost}, actions.Methods)
	assert.Equal(t, webui.AuthAuthenticated, actions.Auth)
	assert.Nil(t, actions.Page, "actions returns JSON, not an HTML document")
	assert.NotNil(t, actions.Handler, "a raw http.Handler must be set")

	live := routes[3]
	assert.Equal(t, webui.OriginTrusted, live.Origin)
	assert.Equal(t, "/agent-ui/{ns}/{name}/live", live.Pattern)
	assert.Equal(t, []string{http.MethodGet}, live.Methods)
	assert.Equal(t, webui.AuthAuthenticated, live.Auth)
	assert.Nil(t, live.Page, "live is a websocket upgrade, not an HTML document")
	assert.NotNil(t, live.Handler, "a raw http.Handler must be set")
}

// TestName_ReturnsAgentUI proves the registry key + route pattern agree — a
// mismatch here would let the WebUI register under one name while serving
// paths implying another.
func TestName_ReturnsAgentUI(t *testing.T) {
	assert.Equal(t, "agent-ui", agentui.New().Name())
}
