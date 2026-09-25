package sessions

import (
	"context"
	"errors"
	"net/http"

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// shellProps is the bootstrap shape the `sessions` React app consumes; its
// json tags are the wire contract with that app. list.go declares and builds
// the list halves (sessionRow, listNotices, startableClass); view.go declares
// and builds Selected (selectedView, viewFor). This file only wires them in.
//
// ui/testdata/shell.golden.json is shellPageBuild's output for one fixture,
// asserted here (page_golden_internal_test.go) and re-read by
// SessionShell.test.tsx, so a json-tag rename or a value change fails on both
// sides. It is deliberately separate from the agent-UI view's
// props.golden.json: the two are produced by different builders, and merging
// them would make an edit to either contract churn both.
type shellProps struct {
	// Subject is the viewer's identity in DISPLAY form
	// (identity.DecodeForDisplay), never the canonical SpiceDB subject:
	// the identity line is user-facing copy, the canonical form is internal
	// vocabulary. Same decode pkg/web/webui/agentui's page does for its chrome.
	Subject string `json:"subject"`
	// Sessions is the sidebar list, newest start first.
	Sessions []sessionRow `json:"sessions"`
	// Notices is how the list is incomplete; zero values mean complete.
	Notices listNotices `json:"notices"`
	// Selected is the `?session=` selection's resolved view; omitted when the
	// request names none.
	Selected *selectedView `json:"selected,omitempty"`
	// StartableClasses is the (namespace, class) set the viewer may start a
	// session for, derived from Sessions: a viewer may start a session for a
	// class they already interact with a session of, in that namespace. Empty
	// means the start control is not rendered — there is nothing it could
	// authorize.
	StartableClasses []startableClass `json:"startableClasses"`
	// CanStartSessions reports whether THIS webd mounted the start route at
	// all — false whenever StartBrowserSession() was nil at Routes() time.
	//
	// A SEPARATE fact from StartableClasses being non-empty, and both are
	// required before the shell renders a start control: a viewer can hold
	// standing on several agents on a webd that mounts no start route (the
	// ordinary shared-cluster shape), and rendering from standing alone would
	// offer a button whose every press 404s.
	CanStartSessions bool `json:"canStartSessions"`
}

// shellPageBuild is the Page.Build for GET /sessions. It carries no BLANKET
// authorization gate (see sessions.go's Routes for why): the session LIST
// itself — buildSessionList, via LookupInteractableSessions — is the
// per-subject authorization boundary for the sidebar. A `?session=<ns>/<name>`
// selection is a narrower question — "may THIS viewer interact with THIS
// session" — gated explicitly below before anything about it is resolved.
func shellPageBuild(d Deps) func(context.Context, *http.Request) (any, webui.PageMeta, error) {
	return func(ctx context.Context, r *http.Request) (any, webui.PageMeta, error) {
		subject := webui.SubjectFromContext(ctx)

		// include widens buildSessionList's lookup-authorized set by exactly the
		// selected session, for the case where the lookup (MinimizeLatency) has
		// not caught up with a just-written started_by (list.go). This is its
		// ONLY caller, and it is set ONLY after the fully-consistent
		// CheckInteract gate below succeeds, from the same (ns, name) that gate
		// was asked about. That gate is what makes passing a browser-supplied
		// selection here safe; without it this is a door into listing a session
		// the viewer was never authorized to see.
		var include *spicedb.SessionRef
		var selected *selectedView

		// Has, not a `raw != ""` check on Get: an empty `?session=` value is a
		// malformed link (400, below), while an absent key is the ordinary
		// no-selection request. Get cannot tell them apart; Has can.
		if r.URL.Query().Has("session") {
			ns, name, ok := splitSessionRef(r.URL.Query().Get("session"))
			if !ok {
				// Answered, not silently dropped into "no selection": the viewer
				// followed a link that does not parse and deserves to know,
				// rather than landing on an unexplained bare list.
				return nil, webui.PageMeta{}, &webui.PageError{Status: http.StatusBadRequest, Kind: "badRequest",
					Title: "Invalid session link", Message: "That session link is malformed."}
			}

			// The per-session door is a GATE, not a list read, so it runs
			// FullyConsistent — internal/cmd/webd's CheckInteract adapter always binds
			// true (see Deps) — unlike buildSessionList's MinimizeLatency
			// sidebar read below.
			allowed, err := d.CheckInteract(ctx, ns, name, subject)
			if err != nil {
				// An indeterminate result is never a denial: 500, not 403.
				// webui.Server's renderAuthorizeFailure collapses a BARE error
				// to 403, so returning a typed *webui.PageError is what keeps
				// this door out of that trap.
				d.Logger().Error(err, "sessions: CheckInteract errored for the selected session; returning 500",
					"ns", ns, "name", name, "subject", subject)
				return nil, webui.PageMeta{}, &webui.PageError{Status: http.StatusInternalServerError, Kind: "error",
					Title: "Authorization error", Message: "Could not verify access to this session."}
			}
			if !allowed {
				return nil, webui.PageMeta{}, &webui.PageError{Status: http.StatusForbidden, Kind: "forbidden",
					Title: "Access denied", Message: "You do not have access to this session."}
			}

			ref := spicedb.SessionRef{Namespace: ns, Name: name}
			include = &ref

			sv, pe := viewFor(ctx, d, ns, name, r.URL.Query().Get("view"))
			if pe != nil {
				return nil, webui.PageMeta{}, pe
			}
			selected = &sv
		}

		rows, notices, err := buildSessionList(ctx, d, subject, include, false /* MinimizeLatency: a sidebar read, not a gate */)
		if err != nil {
			if errors.Is(err, errEmptySubject) {
				// Unreachable in production (see errEmptySubject). A real 401
				// rather than a panic or a silent empty list, so a caller that
				// DOES reach it gets a diagnosable answer.
				return nil, webui.PageMeta{}, &webui.PageError{Status: http.StatusUnauthorized, Kind: "unauthorized",
					Title: "Sign in required", Message: "You must be signed in to view this page."}
			}
			// A failed lookup/join is an error page, never an empty list: an
			// empty Sessions slice is indistinguishable from "you have no
			// sessions", the silent-failure shape this repo forbids.
			d.Logger().Error(err, "sessions: shell page failed to build the session list; returning 500",
				"subject", subject)
			return nil, webui.PageMeta{}, &webui.PageError{Status: http.StatusInternalServerError, Kind: "error",
				Title: "Could not load your sessions", Message: "Try again in a moment."}
		}

		classes, notices := startableClassesFor(ctx, d, subject, rows, notices, false /* first paint */)

		return shellProps{
			Subject:          identity.DecodeForDisplay(subject),
			Sessions:         rows,
			Notices:          notices,
			Selected:         selected,
			StartableClasses: classes,
			// The SAME collaborator check Routes uses to decide whether to mount
			// POST /sessions/api/start (sessions.go), asked here so the shell and
			// the route can never disagree about whether starting is possible.
			CanStartSessions: d.StartBrowserSession() != nil,
		}, webui.PageMeta{Title: "Sessions"}, nil
	}
}
