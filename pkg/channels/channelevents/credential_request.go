package channelevents

// KindCredentialRequest is the envelope kind that delivers a "go link your
// credentials" prompt to a session's starter. Channelsd publishes it on
// the credential_request sub-channel when the operator parks a session
// in AwaitingCredentials.
const KindCredentialRequest Kind = "credential_request"

// CredentialRequestPayload is the data the channel kind's
// credential_request sub-channel sender renders into the user-facing
// prompt.
//
// Two link surfaces coexist: LinkButtons (per-credential, preferred whenever
// non-empty) and LinkURL (one signed deep-link covering the PAT-style
// credentials). A sender that renders only LinkURL still works.
type CredentialRequestPayload struct {
	// SessionRef identifies the parked session ("<ns>/<name>") — included
	// for traceability; not user-facing.
	SessionRef string `json:"sessionRef"`
	// RecipientCanonical is the canonical SpiceDB subject of the user the
	// prompt should target (the session's starter). Slack's sender uses
	// it to pick the right ephemeral / DM recipient.
	RecipientCanonical string `json:"recipientCanonical"`
	// LinkURL is the single signed deep-link the watcher mints for PAT-style
	// credentials, and the fallback for senders that don't render
	// LinkButtons. Empty for an OAuth-only session.
	//
	// Render as a button — never as plain text — so the URL isn't leaked
	// into thread history.
	LinkURL string `json:"linkURL"`
	// Items is one entry per missing credential, in stable order — the
	// COMPLETE set. Callers MUST include every unconnected credential, never
	// a representative subset and never a truncated list: the prompt's whole
	// job is to enumerate every account the user needs to link, and an
	// incomplete Items silently breaks that.
	Items []CredentialRequestItem `json:"items"`
	// LinkButtons is the per-credential action list: one entry per OAuth
	// credential URL'd to identityd's /link/oauth/<credname>, plus ONE entry
	// covering all PAT credentials with the signed /link?d=&sig= URL. Senders
	// fall back to LinkURL when this is empty.
	//
	// Invariant: the credentials covered across the buttons (one per OAuth
	// entry, all PATs sharing the single PAT entry) sum to the length of the
	// SessionUserIdentity's MissingCredentials list. No truncation, ever.
	LinkButtons []CredentialRequestButton `json:"linkButtons,omitempty"`

	// Icons maps credName → fully-qualified icon URL
	// (<externalBaseURL>/icon/<credName>), populated by channelsd's
	// CredentialRequestWatcher from externalurl.Provider. Slack fetches these
	// server-side and caches its own copy. An empty value is skipped, and a
	// nil map (no externalBaseURL configured) means text-only rows.
	Icons map[string]string `json:"icons,omitempty"`
}

// CredentialRequestItem is one credential row in the prompt.
type CredentialRequestItem struct {
	// Credential is the final (post-remap) credential name (correlates to
	// LinkButtons / Icons keys).
	Credential string `json:"credential"`
	// Title is the bold service/token display name ("GitHub").
	Title string `json:"title"`
	// Description is the "what is this token" sentence (may be empty).
	Description string `json:"description,omitempty"`
	// Why is the per-row reason (always populated by channelsd before send).
	Why string `json:"why,omitempty"`
}

// CredentialRequestButton is a single rendered button in the Slack (or
// other channel) credential_request message. Slack's Block Kit actions
// block supports up to 25 buttons per row, so a session with many
// missing credentials still renders in one row.
type CredentialRequestButton struct {
	// Label is the user-facing button text, e.g. "Connect Linear" or
	// "Connect your accounts".
	Label string `json:"label"`
	// URL is the click destination. For OAuth: identityd's
	// /link/oauth/<credname>. For PAT: the signed /link?d=&sig=.
	URL string `json:"url"`
	// IconURL is the per-button icon URL (externalBaseURL +
	// /icon/<credName>). Set only for single-credential requests so the
	// Slack message has a concrete service icon next to the button.
	// Empty for multi-credential generic buttons ("Connect your
	// accounts") because no single service icon represents the set.
	IconURL string `json:"iconURL,omitempty"`
}
