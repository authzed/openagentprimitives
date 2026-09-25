// pkg/platform/identityd/oauth_discovery.go — the OAuth discovery + MCPServer
// lookup helpers the OAuth handlers call.
package identityd

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughcatalog"
	mcpoauth "github.com/authzed/openagentprimitives/pkg/tools/mcp/oauth"
)

// findMCPServerForCredential returns the single MCPServer whose auth references
// credName, searched cluster-wide. The implementation lives in
// passthroughcatalog so channelsd can run the same lookup without importing
// identityd; this is only the in-package shim.
func findMCPServerForCredential(ctx context.Context, c client.Client, credName string) (*spiceboxv1alpha1.MCPServer, error) {
	return passthroughcatalog.LookupMCPServerByCredential(ctx, c, credName)
}

// discoverOAuthMetadata wraps mcpoauth.Discover so tests and E2E scenarios can
// stub discovery at a package-level seam instead of reaching a real provider.
func discoverOAuthMetadata(ctx context.Context, hc *http.Client, mcpServerURL string) (*mcpoauth.Metadata, error) {
	return mcpoauth.Discover(ctx, hc, mcpServerURL)
}

// oauthRedirectURI builds <externalBaseURL>/oauth/callback/<credname>, the
// post-authorization redirect target. Providers byte-match this against the
// DCR-registered list, so oauthRegistrationRedirects must stay in step with it.
// credName is path-escaped here; callers must not pre-encode it.
func oauthRedirectURI(externalBaseURL, credName string) string {
	return strings.TrimRight(externalBaseURL, "/") + "/oauth/callback/" + url.PathEscape(credName)
}

// agentOAuthRedirectURI builds <externalBaseURL>/oauth/agent-callback/<credname>
// — the agent-owned (workshop-credential) flow's OWN redirect target, distinct
// from oauthRedirectURI's per-user one.
//
// This split is security-critical, not cosmetic (see the callers'
// docs for the full rationale): the workshop-credential authorize step is
// reached against an MCPServer the WORKSHOP BUILDER declared, whose
// spec.server.URL is therefore attacker-influenced input the same way the
// builder's prompt is. If the agent flow registered/authorized with the
// per-user redirect_uri, a hostile provider's redirect would land on the
// PER-USER callback, which redeems the code via useridentity.PutOAuthToken —
// filing the bot's token under the clicking starter's own personal identity,
// bypassing the operator relay and the workshop write authority entirely.
// Giving the agent flow its own redirect base means the two callbacks can
// each refuse an entry that didn't come from their own flow (oauthStateEntry
// .AgentIdentityRef) as a second, independent line of defense — but the
// distinct URL is what keeps a hostile provider from reaching the wrong
// callback in the first place.
func agentOAuthRedirectURI(externalBaseURL, credName string) string {
	return strings.TrimRight(externalBaseURL, "/") + "/oauth/agent-callback/" + url.PathEscape(credName)
}
