// pkg/web/webui/sessions/start.go serves POST /sessions/api/start: starting a
// session for a class the viewer already interacts with, from the dashboard,
// with no session selected.
//
// It is the second of two start routes, deliberately. pkg/web/webui/agentui's
// start route authorizes on interact with the ENDED session the viewer is
// already looking at — "start another one of what you were just doing", a
// strictly narrower and equally correct gate for that control. This route
// serves the case that one cannot: no session is selected, so the class must
// be NAMED, and naming it is what makes a per-request authorization necessary.
// Both return the same wire shape (ui/testdata/start.golden.json pins them
// together), so the browser needs one parse helper.
package sessions

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/browsersession"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/agentui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/browserstart"
	"github.com/authzed/openagentprimitives/pkg/web/webui/chat"
)

// maxStartBody bounds POST /sessions/api/start request bodies — the body
// carries an opening prompt and two short names, never a bulk payload.
// Mirrors pkg/web/webui/agentui's maxStartBody precedent.
const maxStartBody = 64 << 10

// startRequest is the POST body. The namespace and class are named because
// there is no selected session to read them from — and both are re-derived and
// re-authorized below, so naming them here grants nothing.
type startRequest struct {
	// Ns is the namespace to create the session in.
	Ns string `json:"ns"`
	// AgentClass is the class to start; checked as a PAIR with Ns.
	AgentClass string `json:"agentClass"`
	// Prompt is the opening turn; blank is accepted only for a class that
	// declares a view (see openedViaUIPrompt).
	Prompt string `json:"prompt"`
}

// StartResponse addresses the session that was created. Its json tags match
// what pkg/web/webui/agentui's start route returns, and
// ui/testdata/start.golden.json pins the two producers and the browser's
// single parse helper against one another.
type StartResponse struct {
	// Ns is the new session's namespace.
	Ns string `json:"ns"`
	// Name is the new AgentSession's object name.
	Name string `json:"name"`
	// Href is a same-origin RELATIVE path, so a caller navigates without
	// re-deriving the trusted base URL.
	Href string `json:"href"`
}

// Browser-facing refusal copy. Fixed strings: none names a CRD kind, a
// permission, or a Kubernetes object — the real cause goes to the log.
//
// startUnavailableMessage is deliberately ONE message for two different
// server-side facts: a (namespace, class) pair the viewer holds no standing
// on, and a class that does not exist or is not ready. Distinct copy would
// turn this endpoint into a probe for which agents exist on the cluster —
// a caller could enumerate class names by watching which refusal came back.
// startNotStartableHere is DELIBERATELY distinct from startUnavailableMessage.
// It is a statement about this server, not a verdict on the agent or on the
// viewer's access, and conflating the two would tell a viewer who genuinely
// holds standing that they do not. It leaks nothing, because it is only ever
// reached AFTER the standing check above admitted the pair — a caller who
// sees it already knew they hold access to that agent.
const (
	startUnavailableMessage = "That agent is not available to start a session with."
	startNotStartableHere   = "This server cannot start new sessions for that agent. You can still open the sessions you already have with it, or ask an administrator to enable starting here."
	startAuthzUnavailable   = "Could not check your access right now. Try again in a moment."
	startInternalMessage    = "Could not start a new session."
	// startCheckUnavailable answers a control-plane read that did not come
	// back while deciding whether this start is allowed — the cluster
	// settings, the class, the person's open workshops.
	//
	// Its own sentence rather than startAuthzUnavailable's, for the reason
	// startNotStartableHere has its own: that one says the viewer's access
	// could not be checked, which is FALSE here — their standing was
	// established a step earlier, and what failed afterwards was a limit
	// lookup. It names no limit either, because the read that failed is the
	// one that would have told us whether any limit applies to this agent at
	// all.
	startCheckUnavailable = "Could not start a new session right now. Try again in a moment."

	// openedViaUIPrompt is the opening turn for a session started by opening a
	// view rather than by saying something.
	//
	// A STATEMENT OF FACT, never an instruction or a request in the viewer's
	// voice: a session needs a turn 0, and inventing a plausible question the
	// viewer never asked would put words in their transcript and invite the
	// agent to answer a question nobody had.
	//
	// It also tells the agent the one thing it needs to behave: the person is
	// looking at the view, not waiting on a reply. Without that, an agent
	// handed an empty-feeling opening tends to greet, summarize, or ask what is
	// wanted — three turns of noise beside a dashboard that already answered
	// the question.
	openedViaUIPrompt = "The person has opened your view and is looking at it now. " +
		"They have not asked you anything yet — wait for them, and do not greet them or " +
		"summarize the view unless they ask."
	startTooManyForYou = "You have too many sessions open. Close one and try again."
	startAtCapacity    = "This server is at capacity right now. Try again in a moment."
)

// errStartBrowserSessionNil is the error value passed to Logger().Error when
// StartBrowserSession is nil at request time despite the route having been
// mounted. logr's Error signature wants a real error even though the failure
// is a missing collaborator, not an I/O fault; mirrors sessions.go's
// errDepsCastFailed for the same reason.
var errStartBrowserSessionNil = errors.New("sessions: StartBrowserSession is nil")

// errIncompleteStandingList is the error value passed to Logger().Error when
// the gate's own join came back incomplete, so an absent (namespace, class)
// pair cannot be read as a denial. Nothing failed outright — buildSessionList
// already logged one line per dropped row — so this records the DECISION that
// their combined effect must not become a 409.
var errIncompleteStandingList = errors.New("sessions: the viewer's startable-class set is incomplete")

// startHandler serves POST /sessions/api/start.
//
// Gate order, and the reason it is this order:
//
//  1. A subject on the context, and the trusted-Origin pin (CSRF) — the same
//     two the agent-UI POSTs open with.
//  2. Decode the body (bounded) and require a non-blank ns, class and prompt.
//  3. Recompute the viewer's authorized (namespace, class) set from
//     LookupInteractableSessions with fullyConsistent=true, and refuse unless
//     the requested pair is in it. This is the authorization; steps 1 and 2
//     are authentication and shape.
//  4. Refuse a start that would exceed the viewer's builder-workshop cap
//     (workshopCapRefusal, start_workshopcap.go), naming the workshops they
//     already have open. After step 3 so its AgentClass read cannot probe
//     which classes exist; before step 5 so nothing is created first.
//  5. browserstart.Start: reserve a live-session slot (so a capacity refusal
//     happens BEFORE any cluster object exists), create under the viewer's own
//     subject, and adopt so the session has a sink before the browser
//     navigates. Shared with the other start route rather than written twice —
//     losing the reservation half is invisible until a viewer is bounded by
//     nothing.
//
// The body is decoded BEFORE the authorization, unlike the session-scoped
// POSTs: there is no object in the URL to authorize against until the body
// names one. That leaks nothing — a 400 on a malformed body discloses nothing
// about which sessions exist, and the refusal copy for an unauthorized class
// is identical to the copy for an unknown one (startUnavailableMessage), so
// the response cannot be used to probe which classes exist either.
//
// The authorization rule is derived, not declared: the SpiceDB schema has no
// agentclass object type to check a user-level "may start" against, and adding
// one needs a relation AND a reconciler writing its link tuple on every
// reconcile — without that writer every check fails closed silently while the
// button still renders. So the rule is standing the viewer demonstrably holds
// today: they may start (namespace, class) if and only if they may interact
// with at least one existing session of that class in that namespace. The
// PAIR is checked, never the bare class — standing on an agent in one
// namespace must not authorize starting it in another.
func startHandler(d Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		// 1. Authentication + the CSRF origin pin.
		subject := webui.SubjectFromContext(ctx)
		if subject == "" {
			writeStartError(w, d, http.StatusUnauthorized, "You must be signed in to start a session.")
			return
		}
		// webui.TrustedOriginMatch, not an inline compare: it fails closed on a
		// blank trusted origin, which an inline compare treats as a match for
		// every request that sends no Origin at all. This route CREATES
		// objects — the worst place for a CSRF pin that quietly no-ops on a
		// misconfigured deployment.
		if !webui.TrustedOriginMatch(r, d.TrustedOrigin()) {
			d.Logger().Info("sessions start: origin mismatch",
				"origin", r.Header.Get("Origin"), "subject", subject)
			writeStartError(w, d, http.StatusForbidden, "This request did not come from a trusted page.")
			return
		}

		// 2. Shape. A malformed body discloses nothing about which sessions or
		// classes exist, which is why it may be answered before step 3.
		r.Body = http.MaxBytesReader(w, r.Body, maxStartBody)
		var body startRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeStartError(w, d, http.StatusBadRequest, "That request could not be read.")
			return
		}
		ns := strings.TrimSpace(body.Ns)
		class := strings.TrimSpace(body.AgentClass)
		prompt := body.Prompt
		// Shape only: which agent, in which namespace. The MESSAGE is checked
		// after authorization (below), because whether one is required depends
		// on the class, and reading a class here would answer "does this class
		// exist" before step 3 established the caller may ask anything about it.
		if ns == "" || class == "" {
			writeStartError(w, d, http.StatusBadRequest, "That request could not be read.")
			return
		}

		// 3. THE authorization. Recomputed here, server-side, at request time —
		// the list the browser was offered is not an input, and the pair it
		// names is checked rather than trusted.
		allowed, notices, err := authorizedStartableClasses(ctx, d, subject)
		if err != nil {
			// An indeterminate lookup is NEVER a denial (AGENTS.md's
			// no-silent-errors rule): 503, so a SpiceDB outage reads as "ask
			// again" rather than as "you have lost access to every agent".
			d.Logger().Error(err, "sessions start: could not recompute the viewer's startable classes; returning 503",
				"ns", ns, "class", class, "subject", subject)
			writeStartError(w, d, http.StatusServiceUnavailable, startAuthzUnavailable)
			return
		}
		if !containsStartableClass(allowed, ns, class) {
			// ONLY a clean, complete list may produce a refusal. An incomplete
			// one cannot tell "the viewer holds no session of this agent" apart
			// from "the session granting their standing could not be read":
			// buildSessionList drops a row whose Get failed for ANY reason,
			// reports a count rather than a cause, and truncates at
			// maxListedSessions. Reading the narrowed set as a denial would turn
			// an apiserver blip into every viewer being told they have no access
			// — forbidden of the SpiceDB half of this join above, and of the
			// Kubernetes half here.
			//
			// BootstrapUnavailable joins the same test on the other arm: when
			// the platform#start_session check could not be evaluated, an absent
			// pair cannot tell "may not bootstrap" apart from "SpiceDB did not
			// answer", and only the first is a refusal.
			//
			// The log matters as much as the status: the refusal line below
			// asserts the viewer holds nothing, which would be FALSE here, and
			// would send an operator to debug authorization instead of the
			// apiserver.
			if notices.Unavailable > 0 || notices.Truncated || notices.BootstrapUnavailable {
				d.Logger().Error(errIncompleteStandingList,
					"sessions start: the viewer's standing could not be established completely; returning 503 rather than a refusal",
					"ns", ns, "class", class, "subject", subject,
					"unavailable", notices.Unavailable, "truncated", notices.Truncated,
					"bootstrapUnavailable", notices.BootstrapUnavailable)
				writeStartError(w, d, http.StatusServiceUnavailable, startAuthzUnavailable)
				return
			}
			d.Logger().Info("sessions start: refused — the viewer holds no interactable session of this agent in this namespace",
				"ns", ns, "class", class, "subject", subject)
			writeStartError(w, d, http.StatusConflict, startUnavailableMessage)
			return
		}

		// 4. The per-starter builder-workshop cap. AFTER the standing gate,
		// so the AgentClass read it performs cannot be used to probe which
		// classes exist, and BEFORE anything is created, which is the whole
		// point: the operator refuses the same cap (ensureWorkshop), but only
		// once the session exists, so the person lands on a session that
		// boot-failed and is told nothing they can act on.
		//
		// Before the opening-message rule below, deliberately: someone at
		// their ceiling cannot start this session whatever they type, and
		// asking them for a message first would be advice for a problem they
		// do not have.
		capMsg, err := workshopCapRefusal(ctx, d, ns, class, subject)
		if err != nil {
			// Indeterminate, never a verdict — the same rule the standing gate
			// above follows. A refusal would tell someone with no workshops
			// they are at a limit; a start would hand them a session the
			// operator boot-fails seconds later.
			d.Logger().Error(err, "sessions start: could not check the viewer's builder-workshop limit; returning 503",
				"ns", ns, "class", class, "subject", subject)
			writeStartError(w, d, http.StatusServiceUnavailable, startCheckUnavailable)
			return
		}
		if capMsg != "" {
			d.Logger().Info("sessions start: refused — the viewer is at their builder-workshop limit",
				"ns", ns, "class", class, "subject", subject)
			writeStartError(w, d, http.StatusConflict, capMsg)
			return
		}

		// A session always needs an opening prompt: spec.prompt.inline is
		// structurally required and runner.ResolvePrompt errors unless exactly
		// one prompt source is set, so a blank one produces a session with no
		// turn 0 that cannot start at all.
		//
		// For a conversation-only agent the viewer must therefore say
		// something; a conversation with nothing said is not a request. For a
		// view-declaring agent the opposite holds — the viewer HAS stated their
		// request by asking for the view, and demanding prose as well asks them
		// to narrate what they are about to look at. So the platform supplies a
		// sentence that states how the session actually began, rather than a
		// fabricated message dressed as something they typed.
		//
		// Read off the ALREADY-AUTHORIZED list rather than re-reading the
		// AgentClass: it costs no extra read and cannot become a
		// class-existence oracle, being reachable only for a pair this viewer
		// was just proven to hold standing on.
		if strings.TrimSpace(prompt) == "" {
			if !startableClassOffersUI(allowed, ns, class) {
				writeStartError(w, d, http.StatusBadRequest, "A message is required to start a session.")
				return
			}
			prompt = openedViaUIPrompt
		}

		if d.StartBrowserSession() == nil {
			// The route mounts only when this collaborator was non-nil at
			// Routes() time (sessions.go); this handler does not assume that
			// still holds, so a misconfiguration never 500s with nothing in the
			// logs.
			d.Logger().Error(errStartBrowserSessionNil,
				"sessions start: StartBrowserSession is nil at request time", "ns", ns, "class", class, "subject", subject)
			writeStartError(w, d, http.StatusInternalServerError, startInternalMessage)
			return
		}

		// 5. Reserve, create under the viewer's own subject, adopt — the
		// sequence both start routes share, so neither can lose the reservation
		// half (browserstart.Start).
		newName, err := browserstart.Start(ctx, d, browserstart.Params{
			Ns: ns, AgentClass: class, Prompt: prompt, Subject: identity.Subject(subject),
		})
		if err != nil {
			switch {
			case errors.Is(err, browsersession.ErrUnknownAgentClass):
				// SAME copy as the unauthorized case above: distinguishing them
				// would let a caller enumerate which classes exist. Reached ONLY
				// for a verdict on the class (a genuine NotFound, or
				// Valid != True) — browsersession.Create keeps a failed read out
				// of this sentinel, and the default arm answers that with a 500,
				// which discloses nothing about whether the class exists.
				d.Logger().Info("sessions start: agent class is unknown or not yet valid",
					"ns", ns, "class", class, "subject", subject, "err", err.Error())
				writeStartError(w, d, http.StatusConflict, startUnavailableMessage)
			case errors.Is(err, browsersession.ErrNotAnAllowedStarter):
				// SAME status and copy as the unknown-class arm above, on
				// purpose: a distinct answer would tell a caller that the class
				// exists and carries a list. The log line keeps the two apart
				// for an operator.
				d.Logger().Info("sessions start: viewer is not an allowed starter of this class",
					"ns", ns, "class", class, "subject", subject)
				writeStartError(w, d, http.StatusConflict, startUnavailableMessage)
			case errors.Is(err, browserstart.ErrNamespaceNotStartable):
				// The viewer's standing is real — step 3 admitted the pair — and
				// this server simply cannot create there. Its own copy and log
				// line, never the unauthorized-or-unknown refusal above: that one
				// asserts a verdict on the agent, false here, and would send an
				// operator to debug authorization instead of RBAC.
				d.Logger().Info("sessions start: refused — this webd cannot create a session in that namespace",
					"ns", ns, "class", class, "subject", subject, "startableNamespaces", d.StartableNamespaces())
				writeStartError(w, d, http.StatusConflict, startNotStartableHere)
			case errors.Is(err, browserstart.ErrNoBrowserHost):
				d.Logger().Error(err, "sessions start: StartBrowserSession went away between the mount and this request",
					"ns", ns, "class", class, "subject", subject)
				writeStartError(w, d, http.StatusInternalServerError, startInternalMessage)
			default:
				writeStartCapacityError(w, d, err, ns, class, subject)
			}
			return
		}

		writeStartJSON(w, d, http.StatusOK, StartResponse{
			Ns:   ns,
			Name: newName,
			Href: agentui.SessionShellHref(ns, newName),
		})
	})
}

// writeStartCapacityError maps a capacity refusal to its own status and copy.
// The two limits are DIFFERENT problems with different remedies — "you have
// too many open", which the viewer can fix, versus "this server is full",
// which they cannot — so each gets its own status and log line.
//
// Anything else is a 500 with the generic message and an Error-level log: a
// transient control-plane failure (a throttled or timed-out AgentClass read,
// an RBAC refusal) must NOT reuse the 409 above, whose status and copy assert
// a verdict on the agent that is false when the control plane simply did not
// answer. browsersession.Create makes that split (only a NotFound is the
// sentinel), and a 500 leaks nothing about whether the class exists, so the
// shared refusal copy keeps its anti-enumeration property.
//
// The log does not name one step: browserstart.Start returns its
// collaborators' errors unwrapped, so an unrecognized value could have come
// from the reservation OR the create, and picking one would send an operator
// to the wrong place.
func writeStartCapacityError(w http.ResponseWriter, d Deps, err error, ns, class, subject string) {
	switch {
	case errors.Is(err, chat.ErrTooManySessionsForSubject):
		d.Logger().Info("sessions start: refused — the viewer is at their own live-session limit",
			"ns", ns, "class", class, "subject", subject)
		writeStartError(w, d, http.StatusTooManyRequests, startTooManyForYou)
	case errors.Is(err, chat.ErrServerAtCapacity):
		d.Logger().Info("sessions start: refused — this process is at its live-session capacity ceiling",
			"ns", ns, "class", class, "subject", subject)
		writeStartError(w, d, http.StatusServiceUnavailable, startAtCapacity)
	default:
		d.Logger().Error(err, "sessions start: could not start a browser session",
			"ns", ns, "class", class, "subject", subject)
		writeStartError(w, d, http.StatusInternalServerError, startInternalMessage)
	}
}

// containsStartableClass reports whether the (ns, class) PAIR is in the
// viewer's authorized set. The pair, never the bare class: standing on an
// agent in one namespace must not authorize starting the same-named agent in
// another, where it may be an entirely different agent with entirely different
// tools.
func containsStartableClass(allowed []startableClass, ns, class string) bool {
	for _, c := range allowed {
		if c.Ns == ns && c.Class == class {
			return true
		}
	}
	return false
}

// startableClassOffersUI reports whether the named pair declares an
// agent-defined view, per the list the caller was just authorized against.
//
// An absent pair reports false — conservative, and unreachable in practice
// since the only caller checks membership first. False means "require an
// opening message", so an unexpected absence degrades to the stricter rule
// rather than to a session started with nothing said.
func startableClassOffersUI(allowed []startableClass, ns, class string) bool {
	for _, c := range allowed {
		if c.Ns == ns && c.Class == class {
			return c.OffersUI
		}
	}
	return false
}

// startErrorResponse is the refusal body. One field, one fixed message —
// never a wrapped internal error.
type startErrorResponse struct {
	// Error is browser-facing copy: no CRD kind, permission, or object name.
	Error string `json:"error"`
}

// writeStartError writes a refusal. It takes Deps for the same reason
// writeStartJSON does: the struct cannot fail to MARSHAL, but Encode also
// fails on a WRITE — a caller that disconnected mid-refusal — which is a real,
// loggable event. The status is already on the wire, so logging is all that is
// left, and dropping it with `_ =` is the shape this repo forbids.
func writeStartError(w http.ResponseWriter, d Deps, status int, msg string) {
	writeStartJSON(w, d, status, startErrorResponse{Error: msg})
}

func writeStartJSON(w http.ResponseWriter, d Deps, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status is already sent, so there is no answer left to correct —
		// log the cause rather than drop it. The caller sees a truncated body
		// and retries; the operator sees why.
		d.Logger().Info("sessions start: response encode failed", "status", status, "err", err.Error())
	}
}
