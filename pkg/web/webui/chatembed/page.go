package chatembed

import (
	"context"
	"net/http"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// pageProps is the bootstrap the client entry mounts with: the session, and
// nothing a viewer could not already reach.
type pageProps struct {
	Ns   string `json:"ns"`
	Name string `json:"name"`
}

// pageBuild authorizes the viewer on the session named in the path, then
// hands the client exactly that session. 403 on a denial, 500 on an
// indeterminate check — the same shape sessionview's page uses.
func pageBuild(d Deps) func(context.Context, *http.Request) (any, webui.PageMeta, error) {
	return func(ctx context.Context, r *http.Request) (any, webui.PageMeta, error) {
		subject := webui.SubjectFromContext(ctx)
		ns := r.PathValue("ns")
		name := r.PathValue("name")
		ok, err := d.CheckInteract(ctx, ns, name, subject)
		if err != nil {
			d.Logger().Error(err, "chat-embed page: CheckInteract errored; returning 500", "ns", ns, "name", name, "subject", subject)
			return nil, webui.PageMeta{}, &webui.PageError{Status: http.StatusInternalServerError, Kind: "error",
				Title: "Authorization error", Message: "Could not verify access to this conversation."}
		}
		if !ok {
			return nil, webui.PageMeta{}, &webui.PageError{Status: http.StatusForbidden, Kind: "forbidden",
				Title: "Access denied", Message: "You do not have access to this conversation."}
		}
		return pageProps{Ns: ns, Name: name}, webui.PageMeta{Title: "Chat — " + ns + "/" + name}, nil
	}
}
