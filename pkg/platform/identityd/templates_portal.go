// pkg/platform/identityd/templates_portal.go — prop models for the standing-
// portal surface (`/my/accounts`): the already-linked / suggested credential
// lists and the proactive one-credential link form.
//
// These structs are the JSON bootstrap props handed to the React
// `identity-portal` / `identity-link-form` apps via
// webui.AppRenderer.RenderApp, so the JSON tags below MUST match those apps'
// TypeScript prop interfaces (pkg/platform/identityd/ui/portal/types.ts,
// pkg/platform/identityd/ui/linkform/types.ts).
package identityd

// portalPageData is the bootstrap model for GET /my/accounts.
type portalPageData struct {
	// Subject is the canonical the visitor authenticated as; used for link assembly, never rendered.
	Subject string `json:"subject"`
	// DisplayName is Subject in human form (usually its source email), falling back to Subject.
	DisplayName string `json:"displayName"`
	// Linked is the user's currently-stored credentials.
	Linked []portalCredential `json:"linked"`
	// Suggested is what a userPassthrough AgentClass needs and the user has not linked yet.
	Suggested []portalCredential `json:"suggested"`
}

// portalCredential is one row in the linked / suggested lists.
type portalCredential struct {
	// Name is the catalog name embedded in the /my/accounts/<Name>/link and /revoke URLs.
	Name string `json:"name"`
	// Label is the resolved provider name; empty (unresolvable) renders as the bare Name.
	Label string `json:"label"`
	// NeedsRefresh routes this row to /link/oauth/<Name> (the OAuth ceremony)
	// instead of the PAT form — the real question being answered is "can this
	// be re-authorized," not "is this literally type=oauth". For a Linked row
	// it comes from the credkind's NeedsRefresh(); for a Suggested row (no
	// AgentCredential exists yet) it comes from the matching MCPServer's
	// declared auth type, in portalAuthTypeMap.
	//
	// The JSON tag stays isOAuth: it is the wire contract with the
	// identity-portal React app (ui/portal/types.ts, Portal.tsx), which this
	// field rename does not touch — renaming the tag too would need updating
	// those files and rebuilding the webui asset bundle.
	NeedsRefresh bool `json:"isOAuth"`
	// IconURL is the same-origin "/icon/<Name>" endpoint identityd serves.
	IconURL string `json:"iconUrl"`
}

// portalLinkFormData is the bootstrap model for GET /my/accounts/<credname>/link.
type portalLinkFormData struct {
	// CredentialName is the catalog name the form posts back under.
	CredentialName string `json:"credentialName"`
	// CredentialLabel is the human-readable service name shown in the form heading.
	CredentialLabel string `json:"credentialLabel"`
	// Instructions is the provider-sourced how-to-obtain text; empty renders nothing.
	Instructions string `json:"instructions"`
	// DocsURL is the provider's docs link shown beside Instructions; empty renders nothing.
	DocsURL string `json:"docsUrl"`
}
