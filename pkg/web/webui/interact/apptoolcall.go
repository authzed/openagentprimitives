package interact

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/agentcaps"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// maxAppToolCallBody bounds POST /app-tool-call bodies — a tool call's
// arguments are always a small structured payload, never a bulk upload.
// Mirrors maxRequestBody's rationale for /interact.
const maxAppToolCallBody = 64 << 10

// appToolCallTimeout bounds the synchronous webd→runner round-trip: long
// enough for a contained tool execution, short enough that a runner which
// never replies (crashed, wedged, unreachable) doesn't hang the browser
// indefinitely — the widget always gets a structured response within this
// budget (see appToolCallHandler step 7).
const appToolCallTimeout = 30 * time.Second

// appToolCallRequestBody is the wire shape of POST /app-tool-call's body.
// There is deliberately NO "requester" or "requestID" field: the server mints
// Requester from the cookie-verified subject and RequestID itself (step 6),
// because a client-supplied requester would forge a different viewer's
// identity into the runner's audit trail — the same reason interactRequest
// decodes no "via".
type appToolCallRequestBody struct {
	// ToolName is the app-visible MCP tool the widget is calling.
	ToolName string `json:"toolName"`
	// Args is the tool's argument object, forwarded to the runner as-is.
	Args json.RawMessage `json:"args"`
	// ArtifactID names WHICH mounted widget is calling. The shell knows it —
	// it keys its widget frames by artifact id and matches the postMessage
	// source against them — and sends it so the server can look up what that
	// widget IS.
	//
	// Deliberately an id and not an origin: the browser may say which widget,
	// never what it is. The origin is read from the session's own
	// status.activeWidgets (step 5b), so a widget that names a DIFFERENT
	// widget's artifact id pins itself to that other origin — stricter, not
	// laxer — and one that names nothing on this session is refused.
	//
	// Empty for a caller that is not a widget at all (an agent-declared UI
	// binding); the runner leaves those unpinned.
	ArtifactID string `json:"artifactId"`
}

// newAppToolCallRequestID returns a 32-character hex string from 16 random
// bytes, used as AppToolCallRequest.RequestID — the correlation ID threaded
// into the runner's fabricated operation_id (pkg/agent/runner's
// HandleAppToolCall). The runner's own newRequestID is unexported in a package
// this one must not depend on (webui → runner is the wrong direction), so the
// two are duplicated rather than shared.
//
// Panicking on a crypto/rand failure is deliberate: a component that cannot
// generate random correlation IDs is already in deep trouble. Equally
// deliberate is NOT using time or math/rand, which are guessable and could let
// a caller collide another viewer's in-flight request.
func newAppToolCallRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("interact: crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// --- POST /session/{ns}/{name}/app-tool-call ------------------------------

// appToolCallHandler dispatches a browser MCP-UI widget's tool call to the
// runner's synchronous app_tool_call responder (pkg/agent/runner's
// Loop.HandleAppToolCall, via channelevents.RequestIn), through the SAME
// layered, fail-closed gates as interactHandler, checked IN ORDER:
//
//  1. webui cookie auth → subject; empty ⇒ 401. (Defense in depth: the
//     framework's AuthAuthenticated already rejects an unauthenticated
//     request, but repeating the check here lets a unit test exercise it
//     without the full server.)
//  2. Origin pinned to the trusted origin (CSRF) ⇒ mismatch ⇒ 403 — same
//     threat model as interactHandler step 2: a cookie-authenticated,
//     state-changing POST on the trusted origin.
//  3. CheckInteract(ns, name, subject) — the SAME send-authorization gate as
//     /interact (agentsession#interact; never CheckArtifactView, a strict
//     superset that would admit platform admins as participants). An error
//     fails closed as 503, since an authz outage must never read as a 403
//     "denied" decision; a clean "not allowed" is 403.
//  4. session_views must be granted+active on the resolved AgentClass — the
//     SAME class opt-in /interact enforces (handlers.go step 5), independent
//     of step 3, opt-in (defaultOn=false), and fail-closed on a class that
//     cannot be resolved. No interactions-list membership check: an app tool
//     call is not a registered interaction kind, so session_views.interactions
//     has nothing to say about it and Active is the whole gate. The runner
//     re-checks interact and applies its own approval/rate-limit gates, so
//     this exists to stop a class that never enabled browser interaction from
//     having its app-visible MCP tools invoked directly, bypassing the agent
//     loop, by any session participant.
//  5. Decode {toolName, args, artifactId}; malformed JSON or empty toolName
//     ⇒ 400.
//     5b. When artifactId is present, resolve the CALLING widget's MCPServer
//     origin from this session's own status.activeWidgets. An id this session
//     does not have ⇒ 403 (a stale or forged widget must not fall back to an
//     unpinned call); a lookup error ⇒ 503 (unknown identity is not absent
//     identity). Absent artifactId leaves the call unpinned — that is an
//     agent-declared UI binding, not a widget.
//  6. Build the envelope with the SERVER-verified subject as Requester, a
//     server-generated RequestID, and the server-resolved WidgetOrigin; the
//     body carries none of the three.
//  7. channelevents.RequestIn to the runner. A transport failure (timeout,
//     runner unreachable, or a nil NATSRequest, which RequestIn itself turns
//     into an error rather than a panic) is logged and reported to the widget
//     as a structured AppToolCallResponse{Status: error} at HTTP 200: the
//     widget must always receive JSON it can act on, never a hang or a 5xx
//     with no client-side recovery path.
//  8. A reply that fails to unmarshal is handled identically (logged, 200 +
//     Status: error) — a malformed runner reply is this handler's problem to
//     contain, not the widget's.
//
// Every other outcome the runner reports (denied, requires_approval,
// not_found, rate_limited, or a successful ok/IsError result) is relayed
// verbatim at HTTP 200 — the widget reads the body's Status field, never
// the HTTP status code, to decide what happened.
func appToolCallHandler(d Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ns := r.PathValue("ns")
		name := r.PathValue("name")

		// 1. Authentication.
		subject := webui.SubjectFromContext(ctx)
		if subject == "" {
			writeError(w, http.StatusUnauthorized, "not authenticated")
			return
		}

		// 2. Origin pin (CSRF), through the shared webui.TrustedOriginMatch so a
		// blank trusted origin fails closed rather than matching an absent
		// Origin header.
		if !webui.TrustedOriginMatch(r, d.TrustedOrigin()) {
			d.Logger().Info("app-tool-call: origin mismatch", "ns", ns, "name", name, "origin", r.Header.Get("Origin"))
			writeError(w, http.StatusForbidden, "origin not trusted")
			return
		}

		// 3. Send authorization: agentsession#interact, the same gate as
		// /interact (never CheckArtifactView — see interactHandler step 4).
		okInteract, err := d.CheckInteract(ctx, ns, name, subject)
		if err != nil {
			d.Logger().Info("app-tool-call: CheckInteract errored", "ns", ns, "name", name, "subject", subject, "err", err.Error())
			writeError(w, http.StatusServiceUnavailable, "authorization check failed")
			return
		}
		if !okInteract {
			writeError(w, http.StatusForbidden, "not authorized to interact with this session")
			return
		}

		// 4. session_views class opt-in — the same gate /interact enforces
		// (handlers.go step 5), independent of step 3. Without it, a class that
		// never enabled browser interaction would still expose its app-visible
		// MCP tools to direct invocation by any session participant, bypassing
		// the agent loop entirely — a readonly tool auto-runs with no approval.
		// Unresolvable class ⇒ fail closed.
		class, err := d.AgentClassOf(ctx, ns, name)
		if err != nil {
			d.Logger().Info("app-tool-call: AgentClassOf failed; failing closed", "ns", ns, "name", name, "err", err.Error())
			writeError(w, http.StatusForbidden, "session views are not enabled for this session")
			return
		}
		views, verr := agentcaps.ResolveSessionViews(class)
		if verr != nil {
			d.Logger().Info("app-tool-call: session_views grant unresolvable; failing closed", "ns", ns, "name", name, "err", verr.Error())
			writeError(w, http.StatusForbidden, "session views configuration is invalid")
			return
		}
		if !views.Active {
			writeError(w, http.StatusForbidden, "session views are not enabled for this class")
			return
		}

		// 5. Decode the minimal body — toolName + args ONLY.
		r.Body = http.MaxBytesReader(w, r.Body, maxAppToolCallBody)
		var body appToolCallRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if body.ToolName == "" {
			writeError(w, http.StatusBadRequest, "toolName is required")
			return
		}

		// 5b. Resolve the CALLING widget's origin from this session's own
		// status.activeWidgets. The body names an artifact id; the server says
		// what that artifact is. An id this session does not have is refused
		// outright rather than sent unpinned — otherwise "send a bogus artifact
		// id" would be the way around the pin — and a lookup that ERRORS leaves
		// the widget's identity unknown, which is not the same as absent, so it
		// fails closed as 503 the way the CheckInteract outage above does.
		widgetOrigin := ""
		if body.ArtifactID != "" {
			origin, found, oerr := d.WidgetOriginOf(ctx, ns, name, body.ArtifactID)
			if oerr != nil {
				d.Logger().Info("app-tool-call: widget origin lookup errored; failing closed",
					"ns", ns, "name", name, "artifactID", body.ArtifactID, "err", oerr.Error())
				writeError(w, http.StatusServiceUnavailable, "the calling widget could not be identified")
				return
			}
			if !found {
				d.Logger().Info("app-tool-call: no such widget on this session; refusing",
					"ns", ns, "name", name, "artifactID", body.ArtifactID, "subject", subject)
				writeError(w, http.StatusForbidden, "the calling widget is not part of this session")
				return
			}
			widgetOrigin = origin
		}

		// 6. Build the envelope: server-verified Requester, server-generated
		// RequestID, server-resolved WidgetOrigin.
		req := channelevents.AppToolCallRequest{
			ToolName:     body.ToolName,
			Args:         body.Args,
			Requester:    subject,
			RequestID:    newAppToolCallRequestID(),
			WidgetOrigin: widgetOrigin,
		}

		// 7. Synchronous request/reply to the runner.
		reply, err := channelevents.RequestIn(d.NATSRequest(), ns, name, channelevents.KindAppToolCall, req, appToolCallTimeout)
		if err != nil {
			d.Logger().Info("app-tool-call: RequestIn failed", "ns", ns, "name", name, "toolName", body.ToolName, "err", err.Error())
			writeJSON(w, http.StatusOK, channelevents.AppToolCallResponse{
				Status: channelevents.AppToolCallStatusError,
				// ViewerMessage, not Message: this handler AUTHORED this copy for
				// a human and vouches for it, which is exactly the distinction
				// the two fields carry. Message is the operator channel and is
				// stripped before the browser at step 9.
				ViewerMessage: "the agent runner did not respond",
			})
			return
		}

		// 8. Decode the runner's reply; a malformed reply is contained here,
		// not surfaced to the widget as raw bytes or a 5xx.
		var resp channelevents.AppToolCallResponse
		if err := json.Unmarshal(reply, &resp); err != nil {
			d.Logger().Info("app-tool-call: malformed runner reply", "ns", ns, "name", name, "toolName", body.ToolName, "err", err.Error())
			writeJSON(w, http.StatusOK, channelevents.AppToolCallResponse{
				Status:        channelevents.AppToolCallStatusError,
				ViewerMessage: "the agent runner returned an invalid response",
			})
			return
		}

		// 9. Strip the OPERATOR channel before the browser sees it.
		//
		// AppToolCallResponse.Message says so in its own doc: it is written for
		// logs and for the model, and it names SpiceDB permissions, resource
		// types and IDs, CRD field names, and the raw subject. The consumer here
		// is a WIDGET — HTML an MCP server authored — so relaying it verbatim
		// handed a possibly-hostile server a map of the authorization model,
		// delivered by the very control that refused it.
		//
		// ViewerMessage is the half a replying site authored for a human and
		// vouches for; a site with none leaves it empty on purpose, and the
		// renderer substitutes its own fixed copy. So the operator text is
		// DROPPED rather than downgraded into the viewer slot — downgrading is
		// the leak with an extra step.
		//
		// Filtered at this boundary rather than at each replying site: a status
		// added later cannot forget, and the log below keeps the diagnostic.
		if resp.Message != "" {
			d.Logger().Info("app-tool-call: runner reply carried an operator diagnostic; withheld from the widget",
				"ns", ns, "name", name, "toolName", body.ToolName, "status", resp.Status, "message", resp.Message)
			resp.Message = ""
		}
		writeJSON(w, http.StatusOK, resp)
	})
}
