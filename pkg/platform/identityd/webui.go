// pkg/platform/identityd/webui.go — identityd self-registers as a webui.WebUI
// so webd can mount the browser auth surface alongside the other UIs. The
// concrete deps internal/cmd/webd hands the framework must implement WebDeps below; a
// cast failure returns no routes (fail-closed: identity not configured).
package identityd

import (
	"fmt"
	"net/http"
	"os"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	webuiregistry "github.com/authzed/openagentprimitives/pkg/web/webui/registry"
)

// WebDeps is identityd's dependency interface; internal/cmd/webd's concrete deps
// implements it. The method set MUST cover every field identityd.Deps needs
// (K8s, LinkSigner, ExternalBaseURL getter, Authenticators, IconHandler,
// InsecureTrustLinks).
type WebDeps interface {
	K8s() client.Client
	LinkSigner() *passthroughlink.Signer
	ExternalBaseURL() string
	Authenticators() map[string]channelkinds.WebAuthenticator
	IconHandler() http.Handler
	InsecureTrustLinks() bool
}

// WebAuthzDeps is the OPTIONAL half of identityd's deps: the authorization
// client that answers agentidentity#update_credential at click time.
//
// Separate from WebDeps because webd builds its SpiceDB client conditionally
// (nil when SPICEDB_ENDPOINT is unset) and identityd's other surfaces must
// still mount. A deps value not implementing this leaves
// Deps.AgentIdentityAuthz a genuine nil interface, which every agent-owned path
// treats as FAIL-CLOSED.
type WebAuthzDeps interface {
	AgentIdentityAuthz() AgentIdentityAuthz
	// AgentCredentialWriter is the operator client that performs the write —
	// identityd holds no Secret access and never writes one itself.
	AgentCredentialWriter() AgentCredentialWriter
}

// ui is identityd's WebUI. Stateless: the per-process Server (and its
// in-memory state stores) is built inside Routes().
type ui struct{}

// New returns identityd's WebUI for explicit mounting by tests + webd.
func New() webui.WebUI { return ui{} }

func (ui) Name() string { return "identity" }

// Routes builds the identityd Server once — its in-memory state stores then
// live for the process — and returns Server.routes(), the SAME table the
// standalone Handler() mounts, so the two surfaces can never diverge.
func (ui) Routes(deps webui.Deps) []webui.Route {
	d, ok := deps.(WebDeps)
	if !ok {
		fmt.Fprintf(os.Stderr, "identityd: webui deps do not implement identityd.WebDeps (type %T); identityd routes NOT mounted\n", deps)
		return nil // fail-closed: identity not configured
	}
	sd := Deps{
		K8s:                d.K8s(),
		LinkSigner:         d.LinkSigner(),
		ExternalBaseURL:    d.ExternalBaseURL,
		Authenticators:     d.Authenticators(),
		IconHandler:        d.IconHandler(),
		InsecureTrustLinks: d.InsecureTrustLinks(),
	}
	// Optional: only a SpiceDB-configured webd supplies this. Absent, the field
	// stays a genuine nil interface and agent-owned credential updates fail
	// closed (loudly, per request). Announced at mount time too, so an operator
	// sees the degraded surface without waiting for a click.
	if az, ok := deps.(WebAuthzDeps); ok {
		sd.AgentIdentityAuthz = az.AgentIdentityAuthz()
		sd.AgentCredentialWriter = az.AgentCredentialWriter()
	}
	if sd.AgentIdentityAuthz == nil || sd.AgentCredentialWriter == nil {
		fmt.Fprintf(os.Stderr, "identityd: replacing an agent's OWN shared credential will be refused on every click "+
			"(authorization client wired=%t, operator client wired=%t)\n",
			sd.AgentIdentityAuthz != nil, sd.AgentCredentialWriter != nil)
	}
	// Optional, same shape as WebAuthzDeps above: only a SpiceDB-configured
	// webd implements ConsentDeps directly (its ConsentClasses method, built
	// over LookupStartableClasses + LookupInteractableSessions). Absent, the
	// field stays a genuine nil interface and server.go's routes() does not
	// register ANY of the OAuth authorization-server surface.
	if cd, ok := deps.(ConsentDeps); ok {
		sd.Consent = cd
	}
	if sd.Consent == nil {
		fmt.Fprintf(os.Stderr, "identityd: OAuth authorization-server routes "+
			"(metadata/register/authorize/consent) are NOT mounted (no consent-class deps wired)\n")
	}
	return NewServer(sd).routes()
}

func init() { webuiregistry.Register(ui{}) }
