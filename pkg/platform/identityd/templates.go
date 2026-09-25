// pkg/platform/identityd/templates.go — prop models for the /link menu surface.
//
// /link is the unified menu: every credential the parked session needs, each
// row carrying its own status badge (linked / missing) and its own action
// (OAuth → /link/oauth/<credname>, PAT → an inline Save form posting to
// /link/submit). The Slack credential_request button always points here; the
// per-credential variant only changes the button label, not the destination.
//
// These structs are the JSON bootstrap props handed to the React
// `identity-link` app via webui.AppRenderer.RenderApp, so the JSON tags below
// MUST match its TypeScript prop interface
// (pkg/platform/identityd/ui/link/types.ts).
package identityd

// linkPageData is the bootstrap model for GET /link?d=&sig=. The menu lists
// every credential in the SUI's missing-credentials list plus the session
// catalog's already-linked ones, so the user sees their full progress.
type linkPageData struct {
	// SessionRef is the parked AgentSession as "<namespace>/<name>"; the page's heartbeat posts it back.
	SessionRef string `json:"sessionRef"`
	// AgentName renders as the page subtitle; empty omits the subtitle entirely.
	AgentName string `json:"agentName"`
	// What are the sentences explaining why the agent needs credentials, joined into one intro paragraph.
	What []string `json:"what"`
	// SignedLink is the bearer-shaped link this page was opened with, replayed into every PAT form.
	SignedLink string        `json:"signedLink"`
	Rows       []linkMenuRow `json:"rows"`
}

// linkMenuRow is one credential entry in the /link menu.
type linkMenuRow struct {
	// CredentialName is the catalog name the Save form and OAuth URL key on.
	CredentialName string `json:"credentialName"`
	// Label is the human-readable service name; empty falls back to CredentialName.
	Label string `json:"label"`
	// Why is the per-service reason for this credential, shown as a muted sub-line.
	Why string `json:"why"`
	// IconURL is this credential's same-origin "/icon/<credentialName>" endpoint.
	IconURL string `json:"iconUrl"`
	// Status is "linked" (already stored in UserIdentity) or "missing" (still needed).
	Status string `json:"status"`
	// Kind is "oauth" or "pat" — whether the row renders a link or an inline Save form.
	Kind string `json:"kind"`
	// OAuthURL is the per-credential OAuth entry point; empty unless Kind == "oauth".
	OAuthURL string `json:"oauthUrl"`
	// Instructions is the provider-sourced how-to-obtain text; empty renders nothing.
	Instructions string `json:"instructions"`
	// DocsURL is the provider's docs link shown beside Instructions; empty renders nothing.
	DocsURL string `json:"docsUrl"`
}
