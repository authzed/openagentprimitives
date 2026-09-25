// Package onepassword syncs 1Password groups into SpiceDB as
// onepassword_group#member, so an organizational role — engineers, founders —
// is a subject the platform can authorize against.
//
// onepassword_group is a definition of its OWN, distinct from the scaffold's
// `group` that unions it: 1Password owns these groups exclusively and the sync
// engine prunes them, so a hand-made group must not share a definition an
// engine deletes from. The definition TEXT lives in the scaffold
// (pkg/authz/spicedb/schema/schema.zed) because group#member names it and the
// scaffold has to resolve standing alone; ownership is DirectorySyncSource's
// claim below, which is what actually stops anything else from writing what
// this sync deletes.
package onepassword

import (
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
)

// DirectorySyncSource is the relsource identity this kind writes as. It
// claims onepassword_group's member, relhash and label relations EXCLUSIVELY:
// the sync prunes whole groups, and diff-and-prune is only correct when
// nothing else writes what it deletes.
//
// It deliberately does NOT claim the scaffold's `group#member`. That relation
// is unioned to reach these members (pkg/authz/spicedb/schema/schema.zed), but
// it belongs to nobody — a hand-made group must remain writable, and the
// bridging tuple an operator writes to join a `group` to a synced
// onepassword_group (docs/relationshipsource.md) goes through that same
// unclaimed relation.
// onepassword_group#member is a SubjectIdentityClaim because this sync writes
// it with a BARE user subject — onepassword_group:<groupID>#member@user:
// <canonical> (relsync_kind.go's FetchScope, SubjectType "user"). A console
// filtering on that user as the subject matches it directly, so it is exactly
// the shape SubjectIdentityClaims describes.
//
// #relhash is not, and cannot be: its subject is a string digest. Neither is
// #label, for the same structural reason — its subject is the group's
// base64url-encoded displayName on that same permission-less `string` type.
// relsource.Register refuses either as a SubjectIdentityClaim outright
// (checkSubjectIdentityClaims), so this is enforced rather than merely noted.
var DirectorySyncSource = relsource.Source{
	Name:        "onepassworddirectorysync",
	DisplayName: "1Password",
	Claims: []string{
		"onepassword_group#member",
		"onepassword_group#relhash",
		// The display-name label
		// (onepassword_group:<uuid>#label@string:<base64url of displayName>),
		// written by relsync_kind.go's FetchScope as ordinary scope content.
		// Claimed for the sentinel's reason with one extra edge: a label is
		// what the admin console SHOWS a human, so leaving it unclaimed would
		// let any other in-tree writer decide what a synced group is called on
		// the page an operator uses to judge whether the sync is right.
		"onepassword_group#label",
	},
	SubjectIdentityClaims: []string{
		"onepassword_group#member",
	},
}

func init() { relsource.Register(DirectorySyncSource) }
