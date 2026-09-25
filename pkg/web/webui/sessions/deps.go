package sessions

import (
	"context"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/browsersession"
	"github.com/authzed/openagentprimitives/pkg/web/webui/browserstart"
)

// Deps is the session shell's dependency interface. internal/cmd/webd's
// *artifactViewDeps implements it; Routes casts webui.Deps to this and fails
// closed when the cast fails, as pkg/web/webui/agentui does, for the same
// reachable reason: with the viewer prerequisites (SpiceDB, the memory token,
// the operator URL) unconfigured, webd hands the server its identity-only
// *webdDeps umbrella, which carries K8s but no SpiceDB.
//
// One cast (in Routes) backs every ROUTE, so an umbrella missing any
// collaborator here fails them ALL closed together — no partial state where
// the page serves an identity header over a list it cannot build.
//
// view.go's viewFor is the narrower exception: it performs a SECOND,
// per-request `d.(agentui.Deps)` cast for a `?session=` selection, so a Deps
// satisfying THIS interface but not agentui.Deps does mount, does build the
// sidebar, and fails only the selected view (500, logged, never a panic — see
// errViewDepsCastFailed). agentui.Deps' extra collaborators are needed by at
// most one request per page load, and demanding them just to mount the list
// page would be the wrong direction to fail closed in. internal/cmd/webd's umbrella
// satisfies both, so the partial state is theoretical in production.
type Deps interface {
	// LookupInteractableSessions answers which sessions this subject may
	// interact with. The list read passes fullyConsistent=false; the start
	// gate passes true (pkg/authz/spicedb documents why the two differ).
	LookupInteractableSessions(ctx context.Context, canonicalID identity.CanonicalUserID,
		limit uint32, fullyConsistent bool) (spicedb.InteractableSessions, error)
	// CheckInteract gates one named session. Same signature as
	// agentui.Deps.CheckInteract, so internal/cmd/webd's existing adapter — which binds
	// fullyConsistent=true — satisfies both with no second accessor.
	CheckInteract(ctx context.Context, ns, name, subject string) (bool, error)
	// LookupStartableClasses answers the BOOTSTRAP arm of the start gate:
	// which AgentClasses this subject may start a session of, via
	// agentclass#start_session. It exists because the derived arm cannot
	// bootstrap — on a cluster whose first session has never been started,
	// that derivation is empty for everyone.
	//
	// It asks about agentclass, NOT platform, even though platform#start_session
	// is the only arm populated today: the permission resolves through
	// `platform->start_session`, so a future per-agent grant
	// (agentclass#starter) is a relationship write rather than a change here,
	// and checking the platform permission directly would make those two
	// separate gates that drift.
	//
	// One round trip regardless of how many classes the cluster holds, which
	// matters: the list re-derives this on every poll for every open tab.
	LookupStartableClasses(ctx context.Context, canonicalID identity.CanonicalUserID,
		limit uint32, fullyConsistent bool) (spicedb.StartableClasses, error)
	// K8s reads the AgentSession / AgentClass / AgentUI objects the list and
	// the view resolve. Reads only: this plugin creates through
	// browsersession.Create and writes nothing itself.
	K8s() client.Client
	// StartBrowserSession is browsersession's func-typed collaborator, the same
	// type pkg/web/webui/agentui.Deps carries under this name. A FUNC, not an
	// interface, so the nil check is honest. nil means this webd cannot host a
	// browser session, and the shell renders no start control rather than
	// offering one that would 404.
	StartBrowserSession() browsersession.StartFunc
	// TrustedOrigin returns the live trusted-origin base URL, used to pin the
	// start POST's Origin header (CSRF). Same accessor pkg/web/webui/agentui.Deps
	// carries, so internal/cmd/webd's umbrella satisfies both with no second method.
	TrustedOrigin() string
	// StartableNamespaces names the namespaces this webd can CREATE a session
	// in — the namespaced Role granting Channel/Secret/AgentSession writes is
	// installed per namespace, so holding standing on an agent is not the same
	// fact as this server being able to start one for it. An empty set means
	// "nowhere" (browserstart.StartableIn).
	//
	// Two uses that must not be confused: for the DERIVED arm it MARKS the
	// picker's unreachable entries, since a viewer's own standing is not this
	// server's to filter; for the bootstrap enumeration
	// (bootstrapStartableClasses) it BOUNDS the scan, because a class outside
	// it could never be started here and listing one would add a dead control
	// rather than explain one. browserstart.Start refuses on it either way.
	StartableNamespaces() []string
	// WorkshopNamespacesFor is the start gate's dynamic arm — see
	// browserstart.Deps for what it means and why an error fails it closed
	// without touching the static arm above.
	WorkshopNamespacesFor(ctx context.Context, subject string) ([]string, error)
	// LiveSessions returns this process's live-session table, or nil when this
	// webd hosts none. Read PER REQUEST rather than captured once: the table is
	// built during the webui server's UI-mount loop, after webd assembles its
	// deps umbrella, so a captured value would always be nil.
	//
	// An INTERFACE, so the nil check is only honest if the implementation
	// returns a genuine nil interface rather than a typed-nil pointer wrapped
	// in one (AGENTS.md's typed-nil rule) — see internal/cmd/webd's own adapter.
	LiveSessions() LiveSessions
	// Logger backs the fail-closed cast-failure log and every handler's cause log.
	Logger() logr.Logger
}

// LiveSessions is an ALIAS for the shared interface both start routes' one
// reserve/create/adopt sequence needs (pkg/web/webui/browserstart). An alias, not a
// second declaration: internal/cmd/webd implements the method once, and a distinct type
// here would mean the two start routes could be handed two different tables.
type LiveSessions = browserstart.LiveSessions
