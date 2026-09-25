// Package browserstart is the ONE reserve -> create -> adopt sequence behind
// every browser-facing start route.
//
// Two routes start a browser session — POST /sessions/api/start (the dashboard
// control, which NAMES a namespace and class) and POST
// /agent-ui/{ns}/{name}/start (the ended-session control, which reads the class
// from the server's own copy of the session the viewer is already authorized
// on). They differ ONLY in how the (namespace, class) pair is authorized;
// everything after that answer is identical, and getting it wrong is invisible:
// a route that creates a session without reserving a slot first enforces neither
// capacity limit, so a viewer bouncing off its control is bounded by nothing.
//
// Authorization is deliberately NOT here. Each route's gate is its own, and
// folding two different authorization rules into one helper is how a caller ends
// up inheriting a gate that was written for the other one.
package browserstart

import (
	"context"
	"errors"
	"slices"

	"github.com/go-logr/logr"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/browsersession"
)

// LiveSessions is the host process's table of sessions with a live in-process
// sink (a websocket, a listener, a health watcher). Start needs exactly two
// things from it, in this order:
//
//  1. Reserve, BEFORE any cluster object is created, so a capacity refusal
//     leaves nothing behind.
//  2. Adopt, AFTER the create, so the session has somewhere to deliver before
//     the browser navigates to it.
//
// pkg/web/webui/chat's Registry implements it. The interface is declared in the
// consumer so a test can supply a recording fake without standing up a real
// registry — but the caller must still match chat's two exported capacity
// sentinels BY VALUE: two errors.New values with the same text are not
// errors.Is-equal, so a locally re-declared sentinel would silently downgrade
// both refusals to a generic 500.
type LiveSessions interface {
	// Reserve claims a slot for subject under the host's per-subject and
	// process-capacity limits. The returned release drops the reservation and
	// is a no-op once Adopt has published into it.
	Reserve(ns, name, subject string) (release func(), err error)
	// Adopt wires a session the caller has already authorized and created.
	Adopt(ctx context.Context, ns, name, subject string) error
}

// Deps is what Start needs from a route's own deps umbrella. Both
// pkg/web/webui/sessions.Deps and pkg/web/webui/agentui.Deps are supersets of it, so
// internal/cmd/webd's single umbrella satisfies it with no extra accessor.
type Deps interface {
	// StartBrowserSession is browsersession's func-typed collaborator. A FUNC,
	// not an interface, so the nil check is honest: nil means this process
	// cannot host a browser session's live output at all.
	StartBrowserSession() browsersession.StartFunc
	// LiveSessions returns this process's live-session table, or nil when it
	// hosts none. An INTERFACE, so a nil check is only honest if the
	// implementation returns a genuine nil interface rather than a typed-nil
	// pointer wrapped in one (AGENTS.md's typed-nil rule).
	LiveSessions() LiveSessions
	// StartableNamespaces names the namespaces this process can actually
	// CREATE a session's objects in — see StartableIn for what the value means
	// and why it is configuration rather than something derived here.
	StartableNamespaces() []string
	// WorkshopNamespacesFor names the workshop namespaces subject owns — the
	// workshops provisioned for builder sessions they started, currently
	// Ready. The browser may create a session there because the Workshop
	// controller bound this process's start Role into each one
	// (pkg/controllers/workshop.BuildWorkshopBrowserRBAC); the set is derived
	// per request, never configured, because workshops come and go. An error
	// fails closed for the dynamic arm only — see StartableIn.
	WorkshopNamespacesFor(ctx context.Context, subject string) ([]string, error)
	Logger() logr.Logger
}

// ErrNamespaceNotStartable reports that the target namespace is outside the set
// this process can create in. It is a statement about THIS SERVER's reach, never
// a verdict on the viewer's standing or on the agent — each route's own
// authorization has already answered those — so a caller must map it to copy
// that says so, never folding it into the refusal it uses for an unauthorized or
// unknown class. Returned unwrapped, like every error out of Start, so both
// routes' errors.Is match.
var ErrNamespaceNotStartable = errors.New("browserstart: this process cannot create sessions in that namespace")

// StartableIn reports whether ns is startable for subject — the static
// configured set, UNIONED with the dynamic per-subject set.
//
// The static set is CONFIGURATION, not a derivation: creating a browser
// session writes a Channel, a creds Secret and an AgentSession into the target
// namespace, and the RBAC granting those verbs is a namespaced Role an
// operator installs (config/webd/role-default.yaml grants them in "default"
// and nowhere else, deliberately — cluster-wide Secret create/delete on a
// browser-facing pod is standing privilege that outlives every request). This
// process cannot read its own bindings, so it is told what it may create in
// rather than discovering it.
//
// The dynamic set is subject's own Ready workshops (WorkshopNamespacesFor):
// the Workshop controller binds this process's start Role into each one it
// provisions, so this process CAN create there too, and that reach is
// per-subject rather than global — a workshop is reachable only by the viewer
// who owns it. A lookup error is logged and treated as an empty dynamic set:
// it narrows what this call reports rather than widening it, and the static
// arm — unaffected — still answers for everyone.
//
// EMPTY means "nowhere", not "anywhere": the permissive reading would let a
// missing or mistyped value silently restore the dead-end control this exists to
// remove — a picker offering namespaces whose every press 500s at the apiserver.
//
// This resolves WorkshopNamespacesFor ITSELF, once per call — the right shape
// for Start's single-namespace door, which asks about exactly one (subject,
// ns) pair per request. A caller checking MANY namespaces for the same
// subject in one pass (pkg/web/webui/sessions' picker, which asks once per
// distinct row) must resolve the dynamic set ONCE up front instead and use
// StartableInSet — calling this per row would re-issue the same cluster-wide
// Workshop List once per row.
func StartableIn(ctx context.Context, d Deps, subject, ns string) bool {
	static := d.StartableNamespaces()
	if slices.Contains(static, ns) {
		// Skips the workshop lookup entirely: the static arm already answers,
		// which is the common case (e.g. "default"), and costs no round trip.
		return true
	}
	owned, err := d.WorkshopNamespacesFor(ctx, subject)
	if err != nil {
		d.Logger().Info("browserstart: could not resolve the viewer's workshops; only the static namespaces are startable",
			"subject", subject, "ns", ns, "err", err.Error())
		return false
	}
	return StartableInSet(static, owned, ns)
}

// StartableInSet is StartableIn's union check — static namespaces OR subject's
// own resolved workshop namespaces — for a caller that has ALREADY resolved
// the dynamic set once (typically via WorkshopNamespacesFor) rather than
// asking StartableIn to re-resolve it per namespace checked. The two must
// never diverge, so StartableIn is written in terms of this for its own
// fallback check; changing what "startable" means only ever happens here.
func StartableInSet(static, owned []string, ns string) bool {
	return slices.Contains(static, ns) || slices.Contains(owned, ns)
}

// Params names the session to start. Subject is the viewer's own — never a
// service identity: the created session's started_by is what makes it
// interactable by the person who asked for it.
type Params struct {
	Ns         string
	AgentClass string
	Prompt     string
	Subject    identity.Subject
}

// ErrNoBrowserHost reports that this process cannot host a browser session's
// live output — StartBrowserSession is nil. Both routes mount only when it is
// non-nil, so reaching this is a misconfiguration (or a collaborator that went
// away between mount and request), never a viewer's doing. Returned rather than
// logged here so the caller can log it with its own route's context.
var ErrNoBrowserHost = errors.New("browserstart: this process cannot host a browser session")

// Start reserves a live-session slot, creates the session, and adopts it.
//
// It returns the created session's name. Every error it returns is the
// COLLABORATOR's own — a Reserve refusal (chat.ErrTooManySessionsForSubject /
// chat.ErrServerAtCapacity), a Create failure
// (browsersession.ErrUnknownAgentClass), or ErrNoBrowserHost — unwrapped, so a
// caller's errors.Is still matches and each route keeps its own status and copy.
//
// An Adopt failure is deliberately NOT an error: the session exists, it is the
// viewer's, and it is addressable — the next page load wires it on demand.
// Deleting it because the in-process wiring failed is how a session vanishes out
// from under someone, so this returns success and logs the cause.
//
// THE RESERVATION IS RELEASED ONLY WHEN NOTHING WAS CREATED. That is the whole
// rule, and the reason it is a `defer` keyed on a latch rather than a call at
// each failure site:
//
//   - before the create, a failure left nothing behind, so holding the slot
//     would charge a viewer for a session that does not exist;
//   - after the create, an AgentSession + Channel + creds Secret exist and are
//     the viewer's, and the reservation is the ONLY thing bounding how many of
//     those one viewer can create here. Releasing it on an adopt failure would
//     remove the bound exactly when adopts start failing — a degraded operator
//     that never mints a session's memory-token Secret would make every start
//     answer 200, drop its slot, and leave three objects behind, indefinitely.
//   - a PANIC anywhere in between is recovered by net/http per connection, so
//     the process survives; the defer is what keeps that from leaking a slot.
//
// A slot kept past an adopt failure is not kept forever: the registry's reaper
// reclaims a reservation nothing ever published into (chat's reservationTTL), so
// a run of such failures cannot lock a viewer out for the process's lifetime.
func Start(ctx context.Context, d Deps, p Params) (string, error) {
	start := d.StartBrowserSession()
	if start == nil {
		return "", ErrNoBrowserHost
	}

	// Before the reservation, because a refusal here leaves nothing behind and
	// costs nobody a slot. Checked HERE rather than in each route so neither can
	// lose it: without it an unreachable namespace surfaces as an apiserver
	// Forbidden inside browsersession.Create, which matches no sentinel a route
	// tests and so becomes a generic 500 that explains nothing.
	if !StartableIn(ctx, d, p.Subject.String(), p.Ns) {
		return "", ErrNamespaceNotStartable
	}

	// The name is minted here rather than left to Create because the
	// reservation is keyed by it, and reserving after the create would mean a
	// capacity refusal that has already written a Channel and an AgentSession
	// nobody will ever open.
	name := browsersession.NewSessionName(p.AgentClass)
	subject := p.Subject.String()

	// Read ONCE and reuse: a second call could observe a different table (or a
	// nil one) between the reservation and the adopt, leaving a slot reserved
	// with nothing that will ever publish into it.
	live := d.LiveSessions()
	var release func()
	if live != nil {
		rel, err := live.Reserve(p.Ns, name, subject)
		if err != nil {
			return "", err
		}
		release = rel
	} else {
		// No live-session table here: the session is still created and
		// addressable, it simply has no in-process sink until something attaches.
		// Logged rather than skipped silently, because it also means the two
		// capacity limits are not being applied.
		d.Logger().Info("browserstart: this process hosts no live-session table; starting without a reservation or an in-process sink",
			"ns", p.Ns, "class", p.AgentClass, "subject", subject)
	}

	// sessionCreated latches the instant cluster objects exist: it is what the
	// release decision turns on, and the defer is what makes that hold on a panic.
	sessionCreated := false
	defer func() {
		if !sessionCreated && release != nil {
			release()
		}
	}()

	created, err := start(ctx, browsersession.Params{
		Namespace:   p.Ns,
		AgentClass:  p.AgentClass,
		Prompt:      p.Prompt,
		Subject:     p.Subject,
		SessionName: name,
	})
	if err != nil {
		return "", err
	}
	sessionCreated = true
	newName := created.Session.Name

	if live != nil {
		if aerr := live.Adopt(ctx, p.Ns, newName, subject); aerr != nil {
			d.Logger().Error(aerr, "browserstart: the session was created but could not be wired into this process; it stays addressable, keeps its reservation, and will be wired on the next open",
				"ns", p.Ns, "name", newName, "class", p.AgentClass, "subject", subject)
		}
	}
	return newName, nil
}
