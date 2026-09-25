package memory

// The pt-tag mint wire types live here, in the root, rather than in httpsrv,
// because three packages need them and none of them should depend on the other
// two: the server that serves the route, the client that calls it, and the
// operator-side minter that implements it. Putting them in the server package
// would make the client import the server, and the minter import both.

// PtTagMintRequest is the body of POST /memory/_pttag_mint/{ns}/{name}.
//
// Note what is absent: any way to state an audience. The caller says what its
// tool call TOUCHED; the minter decides who may see it. That split is the
// security property of the whole route — pt_tag is ComponentWritten precisely
// so a session cannot author its own reader set, and a request field carrying
// one would hand the pen straight back.
type PtTagMintRequest struct {
	// ToolUseID is the tool_use block whose result carried the datum into
	// context, matching the link infoleakage_taint records under the same name.
	ToolUseID string `json:"toolUseID,omitempty"`

	// Resources are the objects the call read, as SpiceDB type/id pairs. The
	// minter expands each to its authorized subjects. Required for a LEAF mint.
	Resources []PtTagResourceRef `json:"resources,omitempty"`

	// DerivedFrom names existing tags this datum was assembled from. When set,
	// the mint is DERIVED: it takes no resources and no audience of its own,
	// because `reader` resolves the intersection of these through SpiceDB.
	DerivedFrom []string `json:"derivedFrom,omitempty"`

	// UntrustedOrigin marks a leaf whose source may carry injected content.
	// Meaningful only on a leaf mint: integrity is declared where a datum
	// enters and inherited below it through carries_untrusted.
	UntrustedOrigin bool `json:"untrustedOrigin,omitempty"`

	// Content is the datum itself, stored behind the tag so the PLATFORM can
	// resolve it later — to place it in a child's context when a data slot is
	// bound, or to answer an audit. Optional: a tag with no content is still a
	// complete provenance record, it just cannot be handed onward.
	//
	// Note this does NOT put the content anywhere the agent can reach. It
	// lands in pt_tag_content, whose SessionReadable is false precisely so
	// that query_memory — which takes arbitrary Kind names — cannot ask for it
	// straight back.
	//
	// Written in the same call as the tag, never separately: content stored
	// under a tag that failed to mint would be bytes nobody's audience governs,
	// and a tag whose content arrived later would have a window in which a
	// disclosure decision was made about something not yet there.
	Content string `json:"content,omitempty"`

	// MIME describes Content, so a platform forwarding the datum can label it.
	MIME string `json:"mime,omitempty"`
}

// PtTagResourceRef is one object a call read.
type PtTagResourceRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	// Permission is the permission the read was authorized under, so the
	// minter expands the right subject set rather than guessing one.
	Permission string `json:"permission"`
}

// PtTagMintResponse returns the id of the tag that now governs the datum.
type PtTagMintResponse struct {
	TagID string `json:"tagID"`
}

// PtTagVerifyRequest is the body of POST /memory/_pttag_verify/{ns}/{name}.
//
// It exists because the CONTENT-BINDING half of the per-datum egress check —
// "do these bytes actually belong to the tag the payload claims?" — must run
// where the stored bytes live. pt_tag_content is component-written AND
// component-READ (SessionReadable is false), so the runner, holding a session
// bearer, cannot read it over the memory HTTP API to compare for itself; the
// read door refuses a token-originated request by design. Worse, if the runner
// COULD read it, letting the runner do the comparison would defeat the check —
// a compromised runner could pair a witnessed wide id with fabricated bytes and
// simply declare a match. So the runner PARSES the payload's regions (a parse,
// not a privilege) and asks the operator to bind them; the operator holds the
// component credential and returns only WHICH ids bound, never the bytes.
type PtTagVerifyRequest struct {
	// Regions are the nonce-matched pt-untrusted regions the runner parsed out
	// of the outbound payload — each an id the payload claims plus the bytes it
	// carried under that id.
	Regions []PtTagRegion `json:"regions"`
}

// PtTagRegion is one claimed (id, content) pair from an outbound payload.
type PtTagRegion struct {
	ID      string `json:"id"`
	Content string `json:"content"`
}

// PtTagVerifyResponse reports which regions bound to the bytes the platform
// stored for their claimed id.
//
// Verified carries only ids, never content: this route confirms provenance, it
// does not hand a datum back — the caller already has the bytes it sent, and
// returning them would make _pttag_verify a read-back channel for the very
// pt_tag_content the design keeps out of a session's reach.
type PtTagVerifyResponse struct {
	// Verified are the ids whose supplied content matched what the platform
	// stored for them.
	Verified []string `json:"verified,omitempty"`
	// AllBound is true only when EVERY requested region bound. A partially
	// forged payload — one real region beside a fabricated one — is not one the
	// caller can reason about per-datum, so the caller drops the whole payload
	// to the coarse floor rather than honour the regions that happened to match.
	AllBound bool `json:"allBound"`
}

// PtTagResolveRequest is the body of POST /memory/_pttag_resolve/{ns}/{name}.
//
// Unlike _pttag_verify (which confirms ids and returns no bytes), this route
// RETURNS content — the bytes a caller is ENTITLED to receive. It exists for
// the two platform paths that legitimately need a datum's bytes back:
// derive_tag's validator (it must read the source tags it derives from) and a
// subagent's bound data slot (a child needs the parent's bound datum to place
// in its context). Both read pt_tag_content, which is component-read
// (SessionReadable false), so neither can read it over the memory API for
// itself; the operator holds the credential and gates each tag on the caller's
// pt_tag:<id>#access — session + ancestor + granted_to — before returning it.
type PtTagResolveRequest struct {
	// TagIDs are the tags whose content the caller asks for. The scope in the
	// URL is where the bytes live (the caller's own scope for a derive, the
	// PARENT scope for a bound slot); entitlement is per-tag, not per-scope.
	TagIDs []string `json:"tagIDs"`
}

// PtTagResolveResponse returns the content for the entitled tags only. A tag the
// caller lacks pt_tag:<id>#access to, or whose bytes are not in the named scope,
// is simply ABSENT — the route never says which of the two it was, and never
// returns bytes for a tag the caller may not receive.
type PtTagResolveResponse struct {
	Contents []PtTagContent `json:"contents,omitempty"`
}

// PtTagContent is one resolved (id, content) datum.
type PtTagContent struct {
	TagID   string `json:"tagID"`
	Content string `json:"content"`
	MIME    string `json:"mime,omitempty"`
}
