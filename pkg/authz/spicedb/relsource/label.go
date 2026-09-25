package relsource

// LabelRelation is the human-name relation a directory sync writes alongside
// a scope's content (`<definition>:<id>#label@string:<base64url of the name>`).
//
// Declared HERE for the same reason SentinelRelation is, and it is the same
// shape: SpiceDB has no string-valued relation, so a name rides as a subject
// id on the shared, permission-less `string` type
// (pkg/authz/spicedb/schema/schema.zed's `definition string {}`). This is the
// package every consumer can reach — a reader on the spicedb side cannot
// import pkg/platform/relsync, which imports it.
//
// # It is display data, and nothing may read it as anything else
//
// A label grants nothing and denies nothing, and three independent things keep
// that true rather than one comment asking for it:
//
//   - Its SUBJECT TYPE is `string`, which declares no permission and appears
//     in no permission expression anywhere in the schema. A `label` relation
//     therefore cannot be reached by any `permission` computation, because
//     there is nothing on the far end of it to reach.
//   - Nothing names it in a permission. A definition gaining `relation label:
//     string` gains no arm in any union; the relation exists to be READ BACK
//     by the admin console's Directory panel and by nothing else.
//   - It is CLAIMED by the sync source that writes it, exactly as #relhash is
//     (Source.Claims), so no other writer can mint one and no consumer can be
//     handed a name a different component chose.
//
// The distinction that matters when reading a check: a scope's real
// authorization edges (membership) carry USER or userset subjects and are what
// every permission is computed from. If a change ever needs the label to
// decide something, that is the signal the fact belongs in a real relation,
// not that this one should grow a permission.
const LabelRelation = "label"

// LabelSubjectType is the definition a label's encoded name rides as a subject
// id on — the same permission-less `string` type the hash sentinel uses.
//
// Declared alongside the relation so the two spellings that MUST agree across
// three packages (the kind that writes the tuple, the kind's ScopeLabelBridge
// declaration, and the schema text) come from one place. A typo here fails
// SpiceDB's own subject-type resolution at write time; a typo in three
// scattered string literals fails only on the one path a test happened to
// exercise.
const LabelSubjectType = "string"

// IsLabelRelation reports whether relation is the display-name relation.
//
// The same reader-facing job IsSentinelRelation does, for the same structural
// reason: a caller walking a source's claims looking for tuples that link a
// platform USER must skip it, because its subject is an encoded name by
// construction and a user-subject filter can only ever match nothing. See
// checkSubjectIdentityClaims, which refuses such a declaration at registration
// rather than letting it become a probe that silently returns nothing.
func IsLabelRelation(relation string) bool { return relation == LabelRelation }
