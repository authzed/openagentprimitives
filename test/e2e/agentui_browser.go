//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"
	"github.com/nats-io/nats.go"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/browsersession"
	"github.com/authzed/openagentprimitives/pkg/web/uibindings"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/agentui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/browserstart"

	// Register the four uibindings.Resolver sources the bindings route
	// resolves through (pkg/web/uibindings/registry.Get) — a blank import each,
	// mirroring internal/cmd/webd/main.go's identical set. Without these, EVERY
	// binding on EVERY scenario built on this arm resolves to "this view's
	// data source is not available": registry.Get is a plain map lookup
	// keyed by each resolver's own init()-time Register call, and nothing
	// else in this package's import graph reaches these packages.
	_ "github.com/authzed/openagentprimitives/pkg/web/uibindings/actionstate"
	_ "github.com/authzed/openagentprimitives/pkg/web/uibindings/artifactref"
	_ "github.com/authzed/openagentprimitives/pkg/web/uibindings/memoryref"
	_ "github.com/authzed/openagentprimitives/pkg/web/uibindings/tool"
)

// AgentUIBrowser drives the REAL pkg/web/webui/agentui routes against this
// harness's real collaborators: the envtest client, the harness memory
// backend, the harness SpiceDB CheckInteract, and the in-process runner
// reached over the harness NATS connection (its ui_data_binding/ui_action
// responders — see inprocess_runner_factory.go's subscribeFactoryUIDataBinding
// / subscribeFactoryUIAction). Nothing between a call here and an upstream
// MCP tool is a stub.
//
// It deliberately does NOT reproduce webd's cookie-auth middleware: the
// subject is injected with webui.WithSubjectForTest and the Origin header is
// set to the configured trusted origin, so what this arm proves is the
// PLUGIN's own gate order (CheckInteract, the doors ladder, the declaration
// lookup, the parameter filter), not webd's session cookie. Say so at every
// call site that might be read as end-to-end authentication coverage.
//
// One arm == one httptest.Server bound to one fixed viewer subject: every
// request this arm sends (Page/Bindings/Invoke/Live) carries that same
// subject, injected server-side by a middleware wrapping the plugin's own
// http.Handler routes. A scenario needing several viewers builds several
// arms via AgentUIBrowserFor.
type AgentUIBrowser struct {
	t       TB
	subject string
	server  *httptest.Server
	client  *http.Client

	// deps is the arm's own agentui.Deps, kept so View can call
	// agentui.ViewFor DIRECTLY (not through the httptest.Server) — see View's
	// own doc comment for why the server hop is not what these scenarios
	// exercise.
	deps *arDeps
}

// arDeps is agentui.Deps built directly over the harness's real
// collaborators — the e2e-local equivalent of internal/cmd/webd's *artifactViewDeps
// (see that type's compile-time `var _ agentui.Deps` assertion, the working
// reference this mirrors). Every field is assigned from a harness accessor
// exactly once; per AGENTS.md's typed-nil rule, mem is declared as the
// memory.Memory INTERFACE and assigned from h.Memory() (never a typed-nil
// pointer) — Artifacts/ArtifactRenderBytes are a concrete pointer and a func
// value respectively, so a nil there is honest by construction (deps.go's
// own doc comment on the three shapes this arm's Deps must respect).
type arDeps struct {
	k8sCli client.Client
	spdb   interface {
		CheckInteract(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error)
	}
	logger  logr.Logger
	trusted string
	nc      *nats.Conn
	mem     memory.Memory
	artSvc  *artifacts.Service
}

// Compile-time proof this package builds a genuine agentui.Deps — the same
// discipline internal/cmd/webd's `var _ agentui.Deps = (*artifactViewDeps)(nil)`
// applies. A dropped method here would otherwise silently 404 every route
// this arm claims to exercise (agentui.Routes fails closed on a cast miss).
var _ agentui.Deps = (*arDeps)(nil)

func (d *arDeps) K8s() client.Client            { return d.k8sCli }
func (d *arDeps) Logger() logr.Logger           { return d.logger }
func (d *arDeps) TrustedOrigin() string         { return d.trusted }
func (d *arDeps) NATS() *nats.Conn              { return d.nc }
func (d *arDeps) Memory() memory.Memory         { return d.mem }
func (d *arDeps) Artifacts() *artifacts.Service { return d.artSvc }

// ArtifactRenderBytes: nil, honestly — no scenario built on this arm binds
// an "artifact"-sourced prop (the leads-console fixture's bindings are all
// "tool"-sourced), so the resolver that would call this is never reached.
// A concrete func value is the honest nil-check shape (deps.go's doc
// comment on ArtifactRenderBytes); this is not a stub standing in for a
// real implementation, it is the same "not wired" state internal/cmd/webd's own
// deps carry when the artifact-viewer's prerequisites are unconfigured.
func (d *arDeps) ArtifactRenderBytes() uibindings.ArtifactRenderBytesFunc { return nil }

// StartBrowserSession: nil, honestly — no scenario built on this arm posts
// to the start route (AgentUIBrowserFor wires page/bindings/actions/live
// only), so the collaborator whose absence would omit that route is never
// reached. Same "not wired" shape as ArtifactRenderBytes above, not a stub
// standing in for a real implementation.
func (d *arDeps) StartBrowserSession() browsersession.StartFunc { return nil }

// LiveSessions: nil, for the same reason and with the same honesty. The
// live-session table is what the start route reserves a slot in; no scenario
// on this arm starts a session, and a nil INTERFACE (not a typed-nil pointer
// wrapped in one) is what the reserving code's own nil check expects.
func (d *arDeps) LiveSessions() browserstart.LiveSessions { return nil }

// StartableNamespaces: empty, which means "this process can create nowhere"
// (browserstart.StartableIn). Same honesty as the two above — no scenario on
// this arm creates a session, so claiming a reachable namespace would be a
// stub standing in for a capability nothing here has.
func (d *arDeps) StartableNamespaces() []string { return nil }

// WorkshopNamespacesFor is the start gate's dynamic arm (workshop
// namespaces owned by the viewer). The harness never starts a session
// from the browser dialog in a workshop namespace — the workshop bundles
// call the builder's tools through the builder session itself — so the
// dynamic arm is empty here, exactly as the static one is.
func (d *arDeps) WorkshopNamespacesFor(context.Context, string) ([]string, error) { return nil, nil }

// CheckInteract implements agentui.Deps: strips the "user:" SpiceDB-form
// prefix (mirrors internal/cmd/webd's artifactViewDeps.CheckInteract) and checks
// FULLY CONSISTENT — a just-written relationship (e.g. WriteRel seeding a
// grant moments before Page/Bindings) must be visible immediately, matching
// the GET page's own CheckInteract call (fullyConsistent=true, page.go).
func (d *arDeps) CheckInteract(ctx context.Context, ns, name, subject string) (bool, error) {
	return d.spdb.CheckInteract(ctx, ns, name, identity.CanonicalFromTrusted(strings.TrimPrefix(subject, "user:"), "e2e harness subject"), true)
}

// NATSRequest implements agentui.Deps: adapts *nats.Conn.Request to
// channelevents.RequestFunc, mirroring internal/cmd/webd's artifactViewDeps.NATSRequest
// exactly (same nil-is-unconfigured contract). This is what routes a "tool"
// data-binding's resolve through to the harness's in-process runner over
// ap.session.<ns>.<name>.in.ui_data_binding — see
// subscribeFactoryUIDataBinding, the runner-side responder Task 5 wires.
func (d *arDeps) NATSRequest() channelevents.RequestFunc {
	if d.nc == nil {
		return nil
	}
	nc := d.nc
	return func(subj string, p []byte, timeout time.Duration) ([]byte, error) {
		msg, err := nc.Request(subj, p, timeout)
		if err != nil {
			return nil, err
		}
		return msg.Data, nil
	}
}

// AgentUIBrowserFor returns an arm bound to one viewer subject. subject is a
// raw canonical SpiceDB subject string ("user:<base64…>") — the form
// webui.SubjectFromContext yields and agentui.Deps.CheckInteract takes. Build
// it from CanonicalForFakeEmail, which returns an identity.CanonicalUserID:
// convert explicitly at the call site (subject := "user:" + canonical.String())
// rather than widening this parameter, so the typed-vs-raw identity
// distinction stays visible.
//
// Fails loudly at construction, not per route: agentui.Routes returns nil
// when its Deps cast fails, which would make every scenario 404 with
// nothing explaining it — the exact shipped bug agentui's own doc comment
// describes (see agentui.go's hasLogger fallback). A route list shorter
// than the plugin's declared four is therefore a t.Fatal here, naming which
// collaborator is missing, rather than a mystery 404 three tasks downstream.
func (h *Harness) AgentUIBrowserFor(subject string) *AgentUIBrowser {
	h.t.Helper()

	deps := &arDeps{
		k8sCli: h.K8s,
		spdb:   h.SpiceDB,
		logger: logr.Discard(),
		nc:     h.nc,
		// sysApprovedMem is the harness's own stand-in for the operator
		// httpsrv's per-request capability mint (see its doc comment in
		// harness.go) — h.Memory() is the shared *memory.Local directly,
		// which enforces the SAME capability-in-context door production's
		// *memory.Local backend does regardless of transport, and a bare
		// context reaching it (as agentui.Deps' resolveView/resolveOneBinding
		// callers do) fails closed. Production's internal/cmd/webd reaches memory over
		// an HTTP client that presents a session-scoped bearer token instead
		// (deps.go's own doc comment on the httpclient.Client alternative);
		// this arm has no such HTTP hop, so it mints the same system approval
		// every other in-process harness component uses to clear that door.
		mem:    sysApprovedMem{inner: h.Memory()},
		artSvc: nil,
	}

	routes := agentui.New().Routes(deps)
	var page, bindings, actions, live *webui.Route
	for i := range routes {
		r := &routes[i]
		switch {
		case strings.HasSuffix(r.Pattern, "/live"):
			live = r
		case strings.HasSuffix(r.Pattern, "/bindings"):
			bindings = r
		case strings.HasSuffix(r.Pattern, "/actions"):
			actions = r
		default:
			page = r
		}
	}
	if page == nil || page.Handler == nil || bindings == nil || actions == nil || live == nil {
		h.t.Fatalf("AgentUIBrowserFor: agentui.Routes returned %d route(s), missing one of address/bindings/actions/live "+
			"(got: %v) — the arDeps cast likely failed; check every agentui.Deps method is implemented", len(routes), routeDump(routes))
	}

	// One middleware, shared by bindings/actions/live: injects the arm's
	// fixed subject via webui.WithSubjectForTest, exactly as production's
	// auth middleware would inject a cookie-verified one — see the type
	// doc comment for what this arm does and does not prove about auth.
	withSubject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(webui.WithSubjectForTest(r.Context(), subject)))
		})
	}

	mux := http.NewServeMux()
	mux.Handle(bindings.Pattern, withSubject(bindings.Handler))
	mux.Handle(actions.Pattern, withSubject(actions.Handler))
	mux.Handle(live.Pattern, withSubject(live.Handler))

	server := httptest.NewServer(mux)
	h.t.Cleanup(server.Close)
	deps.trusted = server.URL

	return &AgentUIBrowser{
		t:       h.t,
		subject: subject,
		server:  server,
		client:  server.Client(),
		deps:    deps,
	}
}

// routeDump renders route patterns for a t.Fatalf message.
func routeDump(routes []webui.Route) []string {
	out := make([]string, len(routes))
	for i, r := range routes {
		out[i] = r.Pattern
	}
	return out
}

// Page returns the agent-defined view's props (agentui.ViewProps, as generic
// JSON) or the *webui.PageError the doors ladder produced (returned as a plain
// error — callers that need the status use errors.As).
//
// It calls agentui.ViewFor DIRECTLY — the same entry point the session shell's
// own viewFor calls for a selected session, and the whole of what the server
// decides about this view. The address /agent-ui/{ns}/{name} is a redirect
// into the shell and resolves nothing, so round-tripping it would exercise the
// redirect rather than the resolution these scenarios are about.
//
// The interact gate is NOT re-checked by ViewFor (its caller owns it), so this
// arm checks it here — otherwise a scenario asserting that an unauthorized
// viewer is refused would be asserting nothing.
func (b *AgentUIBrowser) Page(ctx context.Context, ns, name string) (json.RawMessage, error) {
	ctx = webui.WithSubjectForTest(ctx, b.subject)

	ok, err := b.deps.CheckInteract(ctx, ns, name, b.subject)
	if err != nil {
		return nil, fmt.Errorf("agentui browser arm: check interact on %s/%s: %w", ns, name, err)
	}
	if !ok {
		return nil, &webui.PageError{Status: http.StatusForbidden, Kind: "forbidden",
			Title: "Access denied", Message: "You do not have access to this session."}
	}

	props, _, _, pe := agentui.ViewFor(ctx, b.deps, ns, name)
	if pe != nil {
		return nil, pe
	}
	raw, merr := json.Marshal(props)
	if merr != nil {
		return nil, fmt.Errorf("agentui browser arm: marshal view props: %w", merr)
	}
	return raw, nil
}

// BindingResult is this arm's decoded mirror of pkg/web/webui/agentui's
// unexported bindingResult. Re-declared here because that type is
// unexported; every json tag is identical to the server's, and it is
// populated by decoding the SAME bytes the handler wrote (Bindings below),
// never by re-marshalling a value this package constructed itself.
type BindingResult struct {
	Status  string          `json:"status"`
	Value   json.RawMessage `json:"value,omitempty"`
	Message string          `json:"message,omitempty"`
}

type bindingsResponseWire struct {
	Bindings map[string]BindingResult `json:"bindings"`
}

// Bindings POSTs a parameter map to .../bindings and returns the decoded
// response body's Bindings map alongside the raw HTTP status. err is
// non-nil only for a transport failure (dial/read) or malformed JSON —
// a well-formed 4xx/5xx response is returned via status, not err, so a
// caller can assert on a rejection's status without a type switch.
func (b *AgentUIBrowser) Bindings(ctx context.Context, ns, name string, params map[string]string) (map[string]BindingResult, int, error) {
	body, err := json.Marshal(struct {
		Params map[string]string `json:"params,omitempty"`
	}{Params: params})
	if err != nil {
		return nil, 0, fmt.Errorf("agentui browser arm: marshal bindings request: %w", err)
	}
	status, raw, err := b.post(ctx, "/agent-ui/"+ns+"/"+name+"/bindings", body)
	if err != nil {
		return nil, 0, err
	}
	if status != http.StatusOK {
		return nil, status, nil
	}
	var wire bindingsResponseWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, status, fmt.Errorf("agentui browser arm: decode bindings response: %w (body=%s)", err, raw)
	}
	return wire.Bindings, status, nil
}

// ActionResponse is this arm's decoded mirror of pkg/web/webui/agentui's
// unexported actionResponseBody.
type ActionResponse struct {
	RequestID string `json:"requestId"`
	State     string `json:"state"`
	Message   string `json:"message,omitempty"`
}

// Invoke POSTs one declared action to .../actions and returns the decoded
// synchronous answer alongside the raw HTTP status. Same err/status split as
// Bindings.
func (b *AgentUIBrowser) Invoke(ctx context.Context, ns, name, action string, params, inputs map[string]string) (ActionResponse, int, error) {
	body, err := json.Marshal(struct {
		Action string            `json:"action"`
		Params map[string]string `json:"params,omitempty"`
		Inputs map[string]string `json:"inputs,omitempty"`
	}{Action: action, Params: params, Inputs: inputs})
	if err != nil {
		return ActionResponse{}, 0, fmt.Errorf("agentui browser arm: marshal action request: %w", err)
	}
	status, raw, err := b.post(ctx, "/agent-ui/"+ns+"/"+name+"/actions", body)
	if err != nil {
		return ActionResponse{}, 0, err
	}
	if status != http.StatusOK {
		return ActionResponse{}, status, nil
	}
	var resp ActionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return ActionResponse{}, status, fmt.Errorf("agentui browser arm: decode action response: %w (body=%s)", err, raw)
	}
	return resp, status, nil
}

// post issues a POST with the Origin header pinned to the arm's trusted
// origin (the server's own URL) — the CSRF pin gateAgentUIPost checks
// (bindings.go). Returns the raw status + body; a non-2xx is NOT an error
// here (see Bindings/Invoke's own doc comments) — only a transport failure
// is.
func (b *AgentUIBrowser) post(ctx context.Context, path string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.server.URL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("agentui browser arm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", b.server.URL)
	resp, err := b.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("agentui browser arm: do request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("agentui browser arm: read response body: %w", err)
	}
	return resp.StatusCode, raw, nil
}

// LiveFrame is this arm's decoded mirror of pkg/web/webui/agentui's
// liveActionMessage/liveViewMessage union (live.go) — both share one `type`
// discriminator on one socket, so one struct with every field optional
// mirrors the wire shape rather than forcing a caller to pick a type before
// decoding.
type LiveFrame struct {
	Type        string          `json:"type"` // "snapshot" | "event" | "error" | "view"
	Actions     json.RawMessage `json:"actions,omitempty"`
	Action      json.RawMessage `json:"action,omitempty"`
	Hook        string          `json:"hook,omitempty"`
	Declaration json.RawMessage `json:"declaration,omitempty"`
	Message     string          `json:"message,omitempty"`
}

// Live opens the GET .../live socket and returns a channel of decoded
// frames plus a close func. The FIRST frames are the snapshot (action) and
// the open-time view push; later ones are pushes. The returned channel is
// closed when the connection closes (server or client side) or ctx is
// cancelled; a decode failure on one frame is dropped with the read loop
// continuing (mirrors the server's own "skip this frame, keep the socket"
// posture for a malformed envelope).
func (b *AgentUIBrowser) Live(ctx context.Context, ns, name string) (<-chan LiveFrame, func(), error) {
	wsURL := "ws" + strings.TrimPrefix(b.server.URL, "http") + "/agent-ui/" + ns + "/" + name + "/live"
	header := http.Header{}
	header.Set("Origin", b.server.URL)

	dialer := *websocket.DefaultDialer
	conn, resp, err := dialer.DialContext(ctx, wsURL, header)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return nil, nil, fmt.Errorf("agentui browser arm: dial live socket: %w (status=%d)", err, status)
	}

	out := make(chan LiveFrame)
	closed := make(chan struct{})
	closeFn := func() {
		select {
		case <-closed:
		default:
			close(closed)
			_ = conn.Close()
		}
	}
	go func() {
		defer close(out)
		defer closeFn()
		for {
			_, raw, rerr := conn.ReadMessage()
			if rerr != nil {
				return
			}
			var frame LiveFrame
			if uerr := json.Unmarshal(raw, &frame); uerr != nil {
				continue // malformed frame; keep reading, mirrors the server's own per-frame skip
			}
			select {
			case out <- frame:
			case <-closed:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			closeFn()
		case <-closed:
		}
	}()

	return out, closeFn, nil
}
