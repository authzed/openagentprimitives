package spicedb

import (
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
)

// TypedWritesSource identifies the four relations *Client's own typed
// helpers write directly against the underlying client (c.cl), bypassing
// the relsource guard entirely: TouchLineage/DeleteLineage
// (agentsession#parent, agentsession#child — lineage.go),
// TouchAttestedIdentity (github_user#user — attested_identity.go, the only
// object type attestedObjectTypes in pkg/controllers/useridentity maps
// today), and TouchSoleIdentity/DeleteSoleIdentity (github_user#sole_user —
// same file, written only while exactly one subject claims the account and
// withdrawn the moment a second appears; see attested_edge.go's derivation).
//
// This is a deliberate, narrower slice of a much larger set: *Client carries
// many more typed helpers that bypass the guard the same way (TouchOwner,
// TouchInteractParticipant[User], TouchDenied[User], TouchArtifactParent,
// TouchPlatformAdmin, ...). Only these four are claimed here, because
// claiming a relation is about refusing OTHER sources, and these four are
// the ones a guarded writer could otherwise plausibly mint by mistake or by
// a mis-declared toolspec/bootstrap CR — a lineage edge or an identity
// attestation. The rest (agentsession#owner/#participant/#denied,
// artifact#parent/#platform, platform#admin, ...) are left for a future,
// deliberate pass rather than claimed as a side effect of this one.
//
// Registering this Source claims the relations so every OTHER
// relsource-bound writer (relwrites, spicedbbootstrap, e2eharness, ...) is
// refused on them. *Client's own typed methods never consult a RelWriter —
// they call c.cl directly — so the claim never refuses the one writer that
// is actually allowed to write these tuples; see relsource.Source's own doc
// on why an owner's own writes bypassing the guard is fine and expected.
// This Source is therefore never handed to (*Client).Writer; it exists
// solely to occupy these four claims in the registry.
// The two github_user claims ARE user↔external-identity links, and are the
// only ones here: attested_identity.go writes both with a BARE user subject —
// github_user:<providerID>#user@user:<canonical> and the same for #sole_user —
// so a console filtering on that user as the subject matches them directly.
// They are also the ONLY bare-user shape anywhere in the github definition
// family; the directory sync's own memberships all take a github_user userset
// (see that source's note). The two agentsession claims are excluded: their
// subject is another agentsession, not a person.
//
// DisplayName is what the console shows, and "spicedbtypedwrites" would read to
// an admin as an internal component rather than an answer. What actually
// produced these tuples is the UserIdentity reconciler observing a credential
// the person themselves declared — an attestation of WHO they are upstream,
// conferring nothing (see attested_identity.go) — so that is what the row says.
var TypedWritesSource = relsource.Source{
	Name:        "spicedbtypedwrites",
	DisplayName: "Account attestation",
	Claims: []string{
		"agentsession#parent",
		"agentsession#child",
		"github_user#user",
		"github_user#sole_user",
	},
	SubjectIdentityClaims: []string{
		"github_user#user",
		"github_user#sole_user",
	},
}

func init() {
	relsource.Register(TypedWritesSource)
}
