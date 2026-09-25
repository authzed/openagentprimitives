package artifactview

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// viewBinding is an (artifact, session) pair PROVEN to belong together, paired
// with the authorization decision that admitted it. Only bindView can produce
// one, and every handler takes its (ns, sess) from here — never from the
// request — so no route can authorize one artifact and then serve a different
// session's data.
type viewBinding struct {
	artifactID string
	ns, sess   string
	backLink   string
	// revisions is the binding probe's own result, carried so the first
	// consumer (the snapshot / the revision list) does not re-query for what
	// the probe already read.
	revisions []RevisionMeta
}

// sessionRef renders the binding's session as the "<ns>/<name>" form the log
// lines and Deps calls use.
func (b viewBinding) sessionRef() string { return b.ns + "/" + b.sess }

// viewDenial is a refusal from bindView. It carries both renderings the
// artifact-view routes need — the terse body the JSON/websocket handlers pass
// to http.Error, and the styled system page the shell's Page.Build returns —
// so the single choke point can serve every route in that route's own idiom.
type viewDenial struct {
	status      int
	text        string // http.Error body
	pageKind    string // webui.PageError Kind
	pageTitle   string
	pageMessage string
}

// writeHTTP renders the denial for a non-page route (ws, revision, download).
func (d *viewDenial) writeHTTP(w http.ResponseWriter) { http.Error(w, d.text, d.status) }

// pageError renders the denial as the framework's styled system page.
func (d *viewDenial) pageError() *webui.PageError {
	return &webui.PageError{Status: d.status, Kind: d.pageKind, Title: d.pageTitle, Message: d.pageMessage}
}

// denyBadLink is returned when the request carries neither a valid signed link
// nor a well-formed artifactId+sessionRef pair.
func denyBadLink() *viewDenial {
	return &viewDenial{
		status: http.StatusForbidden, text: "invalid or expired link",
		pageKind: "expired", pageTitle: "Link invalid or expired",
		pageMessage: "This live-view link is invalid or has expired.",
	}
}

// denyNoAccess is the single refusal for "this subject may not see this
// artifact here". It covers both a failed CheckView and a failed binding
// probe, deliberately: telling the two apart would tell a caller probing
// session refs which artifact ids exist in which session.
func denyNoAccess() *viewDenial {
	return &viewDenial{
		status: http.StatusForbidden, text: "you do not have access to this artifact",
		pageKind: "forbidden", pageTitle: "Access denied",
		pageMessage: "You do not have access to this artifact.",
	}
}

// denyAuthzError is returned when the authorization check itself failed — a
// 500, never a 403: an unreachable SpiceDB must not read as a denial.
func denyAuthzError() *viewDenial {
	return &viewDenial{
		status: http.StatusInternalServerError, text: "authorization error",
		pageKind: "error", pageTitle: "Authorization error",
		pageMessage: "Could not verify access to this artifact.",
	}
}

// bindView is the single authorization choke point for every artifact-view
// route whose target comes from the request. It resolves the request params,
// checks that the subject may view the artifact, and then RESOLVES the
// artifact's owning session server-side rather than trusting the one the
// caller named.
//
// Why the probe RESOLVES rather than merely validates: artifact ids are globally
// unique (memory.NewID) and ListRevisions reads the head strictly inside
// memory.Scope{ID: ns+"/"+sess}, hard-erroring when the id is not in that scope
// (artifacts.Service.RevisionTree). At most ONE session can survive the probe and
// it is the artifact's own, so verifying the pair and resolving it are the same
// act — and every session-scoped Deps call downstream can then only ever run
// against the session the artifact actually lives in.
//
// FAIL-CLOSED on any error, including a transient store failure indistinguishable
// from a mismatch: an artifact store blip must not open an unbound session. Every
// refusal is logged with the full triple so an operator can tell the two apart.
func bindView(ctx context.Context, av Deps, q url.Values) (viewBinding, *viewDenial) {
	subject := webui.SubjectFromContext(ctx)
	artifactID, sessionRef, backLink, err := resolveViewParams(av, q)
	if err != nil {
		av.Logger().Info("artifactview: no verifiable artifact-view params; refusing",
			"subject", subject, "err", err.Error())
		return viewBinding{}, denyBadLink()
	}
	ns, sess := splitRef(sessionRef)
	// A session ref is exactly "<namespace>/<name>", and neither half may be
	// empty or itself contain a separator: the memory scope id is built by
	// re-joining them, so a malformed ref would silently address a scope that
	// is not the session it appears to name.
	if ns == "" || sess == "" || strings.Contains(sess, "/") {
		av.Logger().Info("artifactview: malformed session ref; refusing",
			"artifactID", artifactID, "sessionRef", sessionRef, "subject", subject)
		return viewBinding{}, denyBadLink()
	}

	ok, err := av.CheckView(ctx, artifactID, subject)
	if err != nil {
		av.Logger().Error(err, "artifactview: CheckView errored; refusing with 500",
			"artifactID", artifactID, "sessionRef", sessionRef, "subject", subject)
		return viewBinding{}, denyAuthzError()
	}
	if !ok {
		return viewBinding{}, denyNoAccess()
	}

	revisions, err := av.ListRevisions(ctx, ns, sess, artifactID)
	if err != nil {
		// Either the artifact does not live in this session (an unbound pair —
		// the case this probe exists for) or the store could not answer. The
		// caller sees one refusal; the log carries which.
		av.Logger().Error(err, "artifactview: artifact does not resolve in the requested session; refusing",
			"artifactID", artifactID, "sessionRef", sessionRef, "subject", subject)
		return viewBinding{}, denyNoAccess()
	}
	return viewBinding{artifactID: artifactID, ns: ns, sess: sess, backLink: backLink, revisions: revisions}, nil
}

// mirrorsAllowed reports whether this subject may receive the SESSION-scoped
// mirrors (conversation, plan, notifications, phase) alongside the artifact —
// i.e. whether they hold agentsession#interact on the bound session. CheckView is
// a strict superset (`view = parent->interact + parent->artifact_org_view +
// platform->view_audit`), so a platform admin — or, on an org-visible session,
// any authenticated user — passes the artifact gate without being a participant.
//
// The session comes from the binding, so this cannot be asked about a session the
// artifact does not live in. An error disables the mirrors (fail-closed) and is
// logged; the artifact's own revision stream is unaffected.
func (b viewBinding) mirrorsAllowed(ctx context.Context, av Deps, subject string) bool {
	ok, err := av.CheckInteract(ctx, b.ns, b.sess, subject)
	if err != nil {
		av.Logger().Error(err, "artifactview: CheckInteract errored; session mirrors disabled for this socket",
			"artifactID", b.artifactID, "sessionRef", b.sessionRef(), "subject", subject)
		return false
	}
	if !ok {
		av.Logger().Info("artifactview: subject may view the artifact but is not a session participant; session mirrors disabled",
			"artifactID", b.artifactID, "sessionRef", b.sessionRef(), "subject", subject)
	}
	return ok
}
