// start.go serves POST /agent-ui/{ns}/{name}/start: the session-resolution
// ladder's third rung (session.go), behind an explicit action rather than a
// side effect of any GET. Nothing that merely RESOLVES a session starts one:
// the ladder answers 410 for a terminal session wherever it is walked
// (walkAgentUIDoors), and the address /agent-ui/{ns}/{name} redirects into the
// shell without reading anything at all. This route is the only way a
// replacement session comes into existence from the agent-UI surface.
package agentui

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/browsersession"
	"github.com/authzed/openagentprimitives/pkg/web/webui/browserstart"
	"github.com/authzed/openagentprimitives/pkg/web/webui/chat"
)

// maxStartBody bounds POST /start request bodies — the body carries only the
// opening prompt (startRequestBody), never a bulk payload. Mirrors
// actions.go's maxActionBody precedent.
const maxStartBody = 64 << 10

// startRequestBody is the POST body: the opening message, and nothing else.
// The AgentClass and the namespace are read from the SERVER's own copy of the
// session named in the URL — a browser-chosen class would be a door into
// starting a session for an agent the viewer has no standing on.
type startRequestBody struct {
	// Prompt is the new session's opening message; blank is rejected with 400.
	Prompt string `json:"prompt"`
}

// startResponse addresses the session that was created. Its json tags match
// what POST /sessions/api/start returns, and start.golden.json pins the two
// producers and the browser's single parse helper against one another.
type startResponse struct {
	// Ns is the new session's namespace — always the ended session's own.
	Ns string `json:"ns"`
	// Name is the newly-minted AgentSession name, never the one in the URL.
	Name string `json:"name"`
	// Href is a same-origin RELATIVE path, so a caller navigates without
	// re-deriving the trusted base URL.
	Href string `json:"href"`
}

// SessionShellHref is the same-origin relative address of a session in the
// session shell — the ONE place either start route's Href is built, so the two
// cannot drift into handing the browser two different addresses for the same
// thing.
//
// It lives here rather than in pkg/web/webui/sessions (whose page it
// addresses) because of the import direction: sessions imports agentui, never
// the reverse, so a helper here is reachable from both.
//
// The selection is a QUERY PARAMETER, not a path segment: the shell's route is
// "/sessions" with "/sessions/api/..." siblings, so a "/sessions/{ns}/{name}"
// shape would make a namespace called "api" unaddressable.
func SessionShellHref(ns, name string) string {
	return "/sessions?session=" + url.QueryEscape(ns+"/"+name)
}

// The browser-facing outcomes of a failed start. Fixed copy: none names a CRD
// kind, a permission or a condition — the real cause goes to the log. The two
// capacity messages differ because their remedies do: one the viewer can act
// on, the other only an operator can.
//
// Naming the agent's readiness is safe HERE, unlike on the dashboard route
// where the same fact is deliberately indistinguishable from a refusal: this
// caller already proved standing on a session of that very class, so the
// message discloses nothing they could not already see.
const (
	startAgentNotReadyMessage = "This agent is not ready to start a new session right now."
	// startNotStartableHereMessage is a statement about THIS SERVER's reach,
	// not about the agent: a replacement session is created in the ended
	// session's own namespace, and the RBAC granting those writes is installed
	// per namespace. Distinct copy, because telling the viewer their agent is
	// not ready would be false and would send them to wait for something that
	// is never going to change.
	startNotStartableHereMessage = "This server cannot start new sessions for that agent. Ask an administrator to enable starting in this space."
	startInternalErrorMessage    = "Could not start a new session."
	startTooManyForYouMessage    = "You have too many sessions open. Close one and try again."
	startAtCapacityMessage       = "This server is at capacity right now. Try again in a moment."
)

// --- POST /agent-ui/{ns}/{name}/start -------------------------------------

// startHandler serves POST /agent-ui/{ns}/{name}/start: the ladder's
// start-fresh rung, behind an explicit action rather than a GET.
//
// THE AUTHORIZATION, stated plainly, because this route CREATES a session: a
// viewer may start a session of class C in namespace N if they hold
// agentsession#interact (fully consistent) on a session of class C in N. This
// route checks that against the ONE session the URL names and reads C from the
// server's own copy of it — a browser-chosen class would be a door into
// starting a session for an agent the viewer has no standing on.
//
// Same rule POST /sessions/api/start derives, applied to a single witness
// instead of recomputed over the viewer's whole interactable set, so this gate
// admits a STRICT SUBSET of what the dashboard route admits. Deliberately not
// re-derived from LookupInteractableSessions: that costs a lookup plus a
// fan-out of Gets to reach a weaker answer, and would newly refuse a viewer
// whose lookup truncates at its cap.
//
// An ENDED session still grants that standing — the point of the control — but
// not unboundedly: step 6's reservation applies both capacity limits.
//
// Gate order:
//
//  1. gateAgentUIPost — auth, trusted-Origin pin, agentsession#interact.
//  2. Get that session; its spec.class is the ONLY source of the new class.
//  3. ResolveSession must answer Ended; Attached/Asleep is 409.
//  4. Decode the bounded body and require a non-blank prompt.
//  5. Confirm this process can host a browser session at all.
//  6. browserstart.Start: reserve, create under the viewer's subject, adopt.
//
// Steps 1-3 run BEFORE the body is read, so an unauthorized caller learns
// nothing about which sessions exist.
func startHandler(d Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ns := r.PathValue("ns")
		name := r.PathValue("name")
		session := ns + "/" + name

		// 1. Shared preamble (auth, CSRF origin pin, CheckInteract) — see
		// gateAgentUIPost's doc comment.
		subject, ok := gateAgentUIPost(w, r, d)
		if !ok {
			return
		}

		// 2. The named session's spec.class is the ONLY source of the new
		// session's AgentClass — never the request body.
		var sess spiceboxv1alpha1.AgentSession
		if err := d.K8s().Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
			if apierrors.IsNotFound(err) {
				writeError(w, http.StatusNotFound, "This session could not be found.")
				return
			}
			d.Logger().Error(err, "agent-ui start: get AgentSession failed", "session", session)
			writeError(w, http.StatusInternalServerError, "Could not load this session.")
			return
		}

		// 3. This rung fires only once the named session has ENDED — reuse
		// ResolveSession, never re-derive the condition. Attached/Asleep means
		// the viewer already has a session at this address.
		res := ResolveSession(&sess)
		if res.Branch != Ended {
			writeError(w, http.StatusConflict, "This session is still active. Reload the page to continue it.")
			return
		}

		// 4. Decode the minimal body — the opening prompt ONLY.
		r.Body = http.MaxBytesReader(w, r.Body, maxStartBody)
		var body startRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if strings.TrimSpace(body.Prompt) == "" {
			writeError(w, http.StatusBadRequest, "a message is required to start a new session")
			return
		}

		// 5. Mounted only when StartBrowserSession was non-nil at Routes()
		// time, but re-checked here so a misconfiguration never 500s with
		// nothing in the logs.
		if d.StartBrowserSession() == nil {
			d.Logger().Error(errStartBrowserSessionNil,
				"agent-ui start: StartBrowserSession is nil at request time", "session", session)
			writeError(w, http.StatusInternalServerError, startInternalErrorMessage)
			return
		}

		// 6. Reserve, create under the viewer's own subject, adopt — the
		// sequence shared with POST /sessions/api/start. The reservation is
		// what applies the per-subject and process-capacity limits; without
		// it, repeated presses of this control are bounded by nothing.
		newName, err := browserstart.Start(ctx, d, browserstart.Params{
			Ns:         ns,
			AgentClass: sess.Spec.Class,
			Prompt:     body.Prompt,
			Subject:    identity.Subject(subject),
		})
		if err != nil {
			writeStartFailure(w, d, err, session, sess.Spec.Class, subject)
			return
		}

		writeJSON(w, http.StatusOK, startResponse{
			Ns:   ns,
			Name: newName,
			Href: SessionShellHref(ns, newName),
		})
	})
}

// writeStartFailure maps a browserstart.Start failure to this route's status
// and copy.
//
// The two capacity limits are DIFFERENT problems with different remedies — the
// viewer can close a session, but cannot empty a full server — so they get
// different statuses and log lines. Both match pkg/web/webui/chat's exported
// sentinels BY VALUE: two errors.New values with identical text are not
// errors.Is-equal, so a locally re-declared sentinel silently downgrades both
// to a generic 500.
//
// Anything unrecognized is a 500 with generic copy and an ERROR log. That is
// where a transient control-plane failure lands (throttled AgentClass read, an
// RBAC refusal), and it must NOT borrow the "this agent is not ready" copy:
// that asserts a verdict on the agent, when the control plane simply did not
// answer. browsersession.Create makes the split — only a NotFound is that
// sentinel — and this arm depends on it.
//
// The log says "could not start" rather than naming a step: browserstart.Start
// returns its collaborators' errors unwrapped, so the value could be the
// reservation's or the create's, and picking one sends an operator astray.
func writeStartFailure(w http.ResponseWriter, d Deps, err error, session, class, subject string) {
	switch {
	case errors.Is(err, browsersession.ErrUnknownAgentClass):
		d.Logger().Info("agent-ui start: agent class is unknown or not yet valid",
			"session", session, "class", class, "subject", subject, "err", err.Error())
		writeError(w, http.StatusConflict, startAgentNotReadyMessage)
	case errors.Is(err, browsersession.ErrNotAnAllowedStarter):
		// SAME status and copy as the unknown-class arm above, on purpose, and
		// for the same reason the dashboard route pairs them: a distinct answer
		// would tell a caller that the class exists and carries a list, which is
		// the enumeration both routes decline to hand out. Info, not Error —
		// this is a verdict the system reached deliberately, not a failure. The
		// log line is what keeps the two apart for an operator.
		d.Logger().Info("agent-ui start: viewer is not an allowed starter of this class",
			"session", session, "class", class, "subject", subject)
		writeError(w, http.StatusConflict, startAgentNotReadyMessage)
	case errors.Is(err, browserstart.ErrNamespaceNotStartable):
		// Not the "agent is not ready" arm: the agent is fine and the viewer's
		// standing is fine — this webd holds no create RBAC in the ended
		// session's namespace, which no amount of waiting or retrying changes.
		d.Logger().Info("agent-ui start: refused — this webd cannot create a session in that namespace",
			"session", session, "class", class, "subject", subject, "startableNamespaces", d.StartableNamespaces())
		writeError(w, http.StatusConflict, startNotStartableHereMessage)
	case errors.Is(err, chat.ErrTooManySessionsForSubject):
		d.Logger().Info("agent-ui start: refused — the viewer is at their own live-session limit",
			"session", session, "class", class, "subject", subject)
		writeError(w, http.StatusTooManyRequests, startTooManyForYouMessage)
	case errors.Is(err, chat.ErrServerAtCapacity):
		d.Logger().Info("agent-ui start: refused — this process is at its live-session capacity ceiling",
			"session", session, "class", class, "subject", subject)
		writeError(w, http.StatusServiceUnavailable, startAtCapacityMessage)
	default:
		d.Logger().Error(err, "agent-ui start: could not start a browser session",
			"session", session, "class", class, "subject", subject)
		writeError(w, http.StatusInternalServerError, startInternalErrorMessage)
	}
}

// errStartBrowserSessionNil gives logr.Error a real error for a missing
// collaborator at request time despite the route being mounted (startHandler
// step 5). Same shape as agentui.go's errDepsCastFailed.
var errStartBrowserSessionNil = errors.New("agentui: StartBrowserSession is nil")
