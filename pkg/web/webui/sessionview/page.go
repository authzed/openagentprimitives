package sessionview

import (
	"context"
	"net/http"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// widgetProp is one bootstrap-time MCP-UI widget.
type widgetProp struct {
	// ArtifactID identifies the persisted widget within this session.
	ArtifactID string `json:"artifactId"`
	// HostURL is an already-minted /mcpui-host address the browser can frame
	// directly: it holds no signing key, so the content-capability token must
	// be minted server-side (shellBuild).
	HostURL string `json:"hostUrl"`
}

// sessionViewProps is the bootstrap shape the session-view React app consumes.
type sessionViewProps struct {
	// Ns and Name address the session; they are all the client's own live
	// websocket needs, and it re-checks CheckInteract itself before upgrading.
	Ns   string `json:"ns"`
	Name string `json:"name"`
	// SandboxOrigin is the origin widget frames load from; "" means widgets
	// cannot be framed safely and none is offered.
	SandboxOrigin string `json:"sandboxOrigin"`
	// ActiveWidgets are the widgets persisted BEFORE this page loaded; ones
	// offered after arrive live over the socket (live.go).
	ActiveWidgets []widgetProp `json:"activeWidgets"`
}

// shellBuild is the Page.Build for /session-view/{ns}/{name}. It runs after
// the auth middleware (subject via webui.SubjectFromContext) and gates on
// CheckInteract — the ONLY authorization check this page uses, never
// CheckArtifactView/CheckView (deps.go). Failures return a *webui.PageError so
// the framework renders a styled system page.
func shellBuild(d Deps) func(context.Context, *http.Request) (any, webui.PageMeta, error) {
	return func(ctx context.Context, r *http.Request) (any, webui.PageMeta, error) {
		subject := webui.SubjectFromContext(ctx)
		ns := r.PathValue("ns")
		name := r.PathValue("name")

		ok, err := d.CheckInteract(ctx, ns, name, subject)
		if err != nil {
			d.Logger().Error(err, "sessionview page: CheckInteract errored; returning 500",
				"ns", ns, "name", name, "subject", subject)
			return nil, webui.PageMeta{}, &webui.PageError{Status: http.StatusInternalServerError, Kind: "error",
				Title: "Authorization error", Message: "Could not verify access to this session."}
		}
		if !ok {
			return nil, webui.PageMeta{}, &webui.PageError{Status: http.StatusForbidden, Kind: "forbidden",
				Title: "Access denied", Message: "You do not have access to this session."}
		}

		return sessionViewProps{
			Ns:            ns,
			Name:          name,
			SandboxOrigin: sandboxOrigin(d),
			ActiveWidgets: activeWidgetProps(ctx, d, ns, name),
		}, webui.PageMeta{Title: "Session view — " + ns + "/" + name}, nil
	}
}

// sandboxOrigin reduces the configured sandbox base URL to a bare
// "scheme://host" origin, or "" when it is not an http(s) origin. One helper
// for every read of Deps.SandboxBaseURL here, so the URL-minting sinks
// (activeWidgetProps, live.go's mintWidgetOfferHostURL) and the props sink
// cannot disagree about what the sandbox origin is.
//
// Failing closed matters because hostUrl becomes the src of an iframe framing
// MCP-server-authored widget HTML. A non-http(s) base would execute in the
// trusted shell origin, and an EMPTY base — the value `oap install` seeds while
// it manages external access, which webd serves with until the real one lands
// — would build a shell-RELATIVE address and frame the widget in the trusted
// origin, collapsing the two-origin split.
func sandboxOrigin(d Deps) string { return webui.StrictOrigin(d.SandboxBaseURL()) }

// activeWidgetProps reads status.activeWidgets and mints a /mcpui-host URL for
// each — the bootstrap set the shell frames on load. A read failure degrades
// to an empty list (logged) rather than failing the whole page: a session-view
// with no widgets is still fully usable, and a widget offered after load still
// arrives live (live.go). A per-widget mint failure skips that widget only.
func activeWidgetProps(ctx context.Context, d Deps, ns, name string) []widgetProp {
	widgets, err := d.ActiveWidgets(ctx, ns, name)
	if err != nil {
		d.Logger().Error(err, "sessionview page: ActiveWidgets errored; bootstrapping with no widgets",
			"ns", ns, "name", name)
		return []widgetProp{} // non-nil: JSON "[]", never "null", on the client
	}
	out := make([]widgetProp, 0, len(widgets))
	base := sandboxOrigin(d)
	if base == "" {
		// No usable sandbox origin: minting anything here would frame widget
		// content in the trusted origin. Degrade to no widgets, the same way
		// an ActiveWidgets read failure does — the page stays usable and the
		// widgets reappear once the origin lands.
		d.Logger().Info("sessionview page: sandbox base URL is not an http(s) origin; bootstrapping with no widgets",
			"ns", ns, "name", name, "sandboxBaseURL", d.SandboxBaseURL())
		return out
	}
	for _, w := range widgets {
		ct, err := d.SignWidgetToken(ns, name, w.ArtifactID)
		if err != nil {
			d.Logger().Error(err, "sessionview page: SignWidgetToken errored; omitting widget",
				"ns", ns, "name", name, "artifactID", w.ArtifactID)
			continue
		}
		out = append(out, widgetProp{ArtifactID: w.ArtifactID, HostURL: base + "/mcpui-host?ct=" + ct})
	}
	return out
}
