package channelevents

// KindPortalAccess is the envelope kind that delivers a "open your
// account portal" link in response to a user's chat trigger
// ("manage my accounts", etc.). Channelsd publishes it on the
// portal_access sub-channel; the receiving channel kind's sender
// renders it as a button to identityd's /my/accounts handler.
const KindPortalAccess Kind = "portal_access"

// PortalAccessPayload is the rendered data for the portal_access
// sub-channel sender. The LinkURL is a passthroughlink-signed deep-link
// whose Payload.Purpose == "portal" (no SessionRef, no
// RequiredCredentials) — identityd's portal handlers verify the
// signature, trust the link's Subject, and set the idd_session cookie
// before rendering /my/accounts.
type PortalAccessPayload struct {
	// RecipientCanonical is the user the portal link is for. The channel
	// kind's sender uses it to pick the right ephemeral / DM target.
	RecipientCanonical string `json:"recipientCanonical"`
	// LinkURL is the full HMAC-signed portal deep-link. Render as a
	// button — never as plain text — so the URL isn't leaked into
	// thread history.
	LinkURL string `json:"linkURL"`
}
