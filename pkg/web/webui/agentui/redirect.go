// redirect.go serves GET /agent-ui/{ns}/{name}: the address an agent-defined
// UI has always been reachable at, now that the view itself is rendered inside
// the session shell.
package agentui

import "net/http"

// redirectHandler answers GET /agent-ui/{ns}/{name} with a redirect to the
// same session in the shell.
//
// The address is kept because it is minted and handed out in places that
// outlive any one page: both start routes answer with an Href from
// SessionShellHref, a channel-side button can carry one, and people assemble
// it by hand.
//
// It holds NO authorization BECAUSE it reads nothing — no Deps, no lookup, and
// an answer that is a pure function of the path. There is no data for a gate
// to protect, and a variant that 404'd for an unknown session would ADD an
// existence oracle an unauthenticated caller could enumerate. The (ns, name)
// echoed back is the caller's own input.
//
// Do not read this AuthNone as the package's posture. Every route reaching
// session DATA runs its own fully-consistent agentsession#interact check —
// gateAgentUIPost for the POSTs, an in-handler check for the live socket — and
// none inherits anything from here. Authorization for what the browser lands
// on is the TARGET's: /sessions is AuthLoginIfNecessary, so an
// unauthenticated visitor goes through login with a `next` back to here, and
// the shell runs CheckInteract on the selection before rendering anything.
func redirectHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// SessionShellHref is the one place either start route builds this
		// address, so the redirect and the JSON a start answers with cannot
		// drift into two different addresses for the same session. It escapes
		// the pair into a single query parameter, so a namespace or name
		// containing a character with meaning in a URL cannot alter the target.
		http.Redirect(w, r, SessionShellHref(r.PathValue("ns"), r.PathValue("name")), http.StatusFound)
	})
}
