package channelevents

// KindCredentialLinked is the envelope kind for the "X just linked"
// out-of-band confirmation. Channelsd publishes it when a credential is
// added or replaced on a UserIdentity, so the legitimate user notices
// in real time if a forwarded link was used by someone else.
//
// This is the detective control that pairs with single-use deep-links to
// cover OIDC's anti-forwarding role in the OIDC-off default:
//   - Mallory grabs Alice's link AFTER Alice already used it → single-use
//     blocks Mallory.
//   - Mallory uses the link BEFORE Alice → Alice sees the credential_linked
//     confirmation, recognises it wasn't her, and revokes.
//
// The envelope is also affirmative feedback for the happy path: a
// legitimate Alice gets "linked X" right after completing the flow.
const KindCredentialLinked Kind = "credential_linked"

// CredentialLinkedPayload is the rendered data for the
// credential_linked sub-channel sender.
type CredentialLinkedPayload struct {
	// RecipientCanonical is the canonical SpiceDB subject the credential
	// was stored under. The channel kind's sender uses this to pick the
	// right ephemeral / DM target.
	RecipientCanonical string `json:"recipientCanonical"`
	// CredentialName is the catalog credential name (e.g. "linear-oauth").
	CredentialName string `json:"credentialName"`
	// CredentialLabel is the human-readable provider label (e.g.
	// "Linear OAuth"). Empty if no MCPServer/Toolkit defined a label —
	// the renderer falls back to CredentialName.
	CredentialLabel string `json:"credentialLabel,omitempty"`
	// SessionRef, when non-empty, names the AgentSession this linking
	// unblocked (the reactive deep-link path). Empty when the credential
	// was linked proactively from the account portal.
	SessionRef string `json:"sessionRef,omitempty"`
}
