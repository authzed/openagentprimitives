package artifactview

import (
	"context"
	"errors"
	"net/http"

	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// artifactViewProps is the bootstrap shape the artifact-view React app consumes.
// It deliberately carries only browser-safe values; the d+sig link is read from
// window.location by the client (the WS + revision endpoints re-verify it).
type artifactViewProps struct {
	ArtifactName        string `json:"artifactName"`        // display title; falls back to the artifact id
	ArtifactDescription string `json:"artifactDescription"` // agent's blurb; may be empty
	// HostURL is the sandbox-origin /artifact-host page the shell frames; the host
	// self-embeds the artifact in an inner no-JS frame. Empty when there is no
	// render yet — the shell shows its generating state and the live ws delivers a
	// HostURL for first framing.
	HostURL       string `json:"hostUrl"`
	ContentURL    string `json:"contentUrl"`    // raw render URL; empty alongside an empty HostURL
	SandboxOrigin string `json:"sandboxOrigin"` // bare scheme://host; "" means "no bridge"
	// OriginLinks are the "back to origin" links derived from the session's
	// channel bindings (a GitHub PR, a Slack thread) — one per distinct origin,
	// input first. Always an array (never null) so the client can map over it.
	OriginLinks []OriginLink `json:"originLinks"`
	// Interactions lists the session_views interaction kinds this session's
	// class grants (e.g. ["user_message"]) — empty when the capability is
	// absent/inactive. The client shows the chat/annotator only when this is
	// non-empty; the real gate is still server-side (the /interact endpoint).
	Interactions []string `json:"interactions"`
	// Ns/Name identify the session for the shell's POST /session/{ns}/{name}/interact.
	// ArtifactID is stamped onto outgoing interaction requests (annotationSend.ts)
	// and re-checked server-side via CheckArtifactView, so the sandbox bundle's
	// own id is never trusted.
	Ns         string `json:"ns"`
	Name       string `json:"name"`
	ArtifactID string `json:"artifactId"`
}

// shellPageBuild is the Page.Build for /artifact-view. It runs AFTER the auth
// middleware (subject available), binds the request through bindView (signed
// link or unsigned admin params, the SpiceDB view check, and the artifact→
// session resolution), resolves the head render, mints a content-capability
// token, and returns props for the React shell. Failures return a
// *webui.PageError so the framework renders a styled system page.
//
// Every session-scoped lookup below (OriginLinks, ArtifactMeta, SessionViews,
// ResolveRender) takes its (ns, sess) from the binding, so the shell's props —
// including the Ns/Name the client stamps onto its /interact posts — can only
// ever name the session the artifact actually lives in.
func shellPageBuild(av Deps) func(context.Context, *http.Request) (any, webui.PageMeta, error) {
	return func(ctx context.Context, r *http.Request) (any, webui.PageMeta, error) {
		b, denial := bindView(ctx, av, r.URL.Query())
		if denial != nil {
			return nil, webui.PageMeta{}, denial.pageError()
		}
		artifactID, ns, sess, sessionRef := b.artifactID, b.ns, b.sess, b.sessionRef()
		// "Back to origin" links derived from the session's channel bindings are
		// SESSION-scoped data — a private Slack thread permalink (team/channel
		// IDs, the fact a DM exists), a private-repo PR URL — so they are gated
		// exactly like the other session mirrors: only a session PARTICIPANT
		// (CheckInteract, via mirrorsAllowed) sees them. CheckView admitted this
		// page, but it is a strict superset of interact, so a platform admin or,
		// on an org-visible session, any authenticated user reaches it without
		// being in the conversation — and must not be handed the origin. A
		// best-effort lookup (OriginLinks never errors), coerced to a non-nil
		// slice so the bootstrap JSON is always an array the client can map over.
		var originLinks []OriginLink
		if b.mirrorsAllowed(ctx, av, webui.SubjectFromContext(ctx)) {
			originLinks = av.OriginLinks(ctx, ns, sess)
		}
		if originLinks == nil {
			originLinks = []OriginLink{}
		}
		title, description := artifactID, ""
		if name, desc, merr := av.ArtifactMeta(ctx, ns, sess, artifactID); merr != nil {
			av.Logger().Error(merr, "artifactview page: ArtifactMeta errored; falling back to id",
				"artifactID", artifactID, "sessionRef", sessionRef)
		} else {
			if name != "" {
				title = name
			}
			description = desc
		}
		// Best-effort: a resolution failure degrades to nil→[] (read-only viewer),
		// the safe default, and is logged by the Deps implementation itself.
		interactions := av.SessionViews(ctx, ns, sess)
		if interactions == nil {
			interactions = []string{}
		}
		renderName, err := av.ResolveRender(ctx, ns, sess, artifactID)
		if err != nil {
			if errors.Is(err, artifacts.ErrNotFound) {
				return nil, webui.PageMeta{}, &webui.PageError{Status: http.StatusNotFound, Kind: "notFound",
					Title: "Artifact unavailable", Message: "This artifact isn't available — it may have expired or been removed."}
			}
			return nil, webui.PageMeta{}, &webui.PageError{Status: http.StatusInternalServerError, Kind: "error",
				Title: "Internal error", Message: "Could not resolve the artifact render."}
		}
		// An empty renderName is "nothing to frame yet" — a not-yet-rendered
		// artifact, or a bundled-only kind (svg/css) whose internal preview child
		// is still generating. The shell mounts with an empty contentUrl and shows
		// its loading state; the live-view ws poll fills it in when content lands.
		var hostURL, contentURL string
		if renderName != "" {
			hostURL, contentURL, err = urlsFor(av, ns, sess, renderName, artifactID)
			if err != nil {
				return nil, webui.PageMeta{}, &webui.PageError{Status: http.StatusInternalServerError, Kind: "error",
					Title: "Internal error", Message: "Could not prepare the artifact content."}
			}
		}
		return artifactViewProps{
			ArtifactName:        title,
			ArtifactDescription: description,
			HostURL:             hostURL,
			ContentURL:          contentURL,
			// Same reduction urlsFor applies (see sandboxOrigin): the client uses
			// this as a postMessage target AND compares it against event.origin, so
			// it must be the bare scheme://host — and "" when the configured value
			// is not one, which the client reads as "no bridge", not "any origin".
			SandboxOrigin: sandboxOrigin(av),
			OriginLinks:   originLinks,
			Interactions:  interactions,
			Ns:            ns,
			Name:          sess,
			ArtifactID:    artifactID,
		}, webui.PageMeta{Title: "Live view — " + title}, nil
	}
}
