package slack

import (
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
)

// DirectorySyncSource identifies the Slack directory sync — the connector
// that keeps slack_workspace/slack_channel/slack_usergroup/slack_user
// membership current in SpiceDB, mirroring Slack's own directory (workspace
// members, channel members, usergroup members, and the slack_user->user
// identity link).
//
// Six of the nine claims below now have an in-tree writer:
// relsync_kind.go's SyncKind (registered as the relsync.Kind "slack") writes
// slack_channel#member, slack_channel#workspace, slack_workspace#member,
// slack_channel#label and slack_user#user directly, and slack_channel#relhash
// indirectly — it's the
// per-scope hash sentinel pkg/platform/relsync/sync.go's Pass emits for
// every relsync.Scope a kind enumerates, and slack_channel is the only
// resource type SyncKind.ListScopes ever hands it.
//
// Three remain claimed but unwritten, and why:
//   - slack_usergroup#member: no usergroups.list/usergroups.users.list call
//     exists anywhere in this kind. A future usergroup sync (its own
//     relsync.Scope resource type, its own ListScopes/FetchScope) is the
//     shape.
//   - slack_workspace#relhash and slack_usergroup#relhash: the #relhash
//     sentinel is scope-level bookkeeping sync.go's Pass emits only for a
//     resource type actually enumerated as a relsync.Scope. slack_workspace
//     and slack_usergroup are never scopes of their own in the shipped
//     kind — slack_workspace#member rides as FetchScope cross-resource
//     content off of slack_channel scopes (sync.go's "Ruling 4"), which
//     carries no sentinel of its own — so neither ever gets one until a
//     kind actually enumerates that resource type as a scope (the same
//     usergroup sync above, or a workspace-scoped roster walk).
//
// All nine stay claimed regardless: an unclaimed relation is writable by
// anything, and the claim is what closes the door on some other writer (an
// MCP toolspec, a SpiceDBBootstrap CR, a test fixture) accidentally minting
// a membership tuple, or forging the sentinel, that only this source's own
// sync engine(s) should ever produce — whether or not one has landed yet
// for a given relation.
// Exactly ONE of the nine claims links a platform user directly:
// slack_user:<uid>#user@user:<canonical> (relsync_kind.go's FetchScope,
// SubjectType "user"). Every membership relation here takes a USERSET subject
// instead — slack_channel:<id>#member@slack_user:<uid>#user, and the same shape
// for slack_workspace#member — matching schema_fragment.go's
// `slack_user#user | slack_bot#agent` union. A user-subject filter can never
// match those, so declaring them would add probes that silently return nothing.
// slack_channel#workspace's subject is a slack_workspace, and the three
// #relhash claims and the #label claim carry a `string` — a digest and an
// encoded name respectively, never a person. relsource.Register refuses either
// as a SubjectIdentityClaim outright (checkSubjectIdentityClaims), so this is
// enforced rather than merely observed here.
var DirectorySyncSource = relsource.Source{
	Name:        "slackdirectorysync",
	DisplayName: "Slack",
	SubjectIdentityClaims: []string{
		"slack_user#user",
	},
	Claims: []string{
		"slack_channel#member",
		"slack_channel#workspace",
		"slack_workspace#member",
		"slack_usergroup#member",
		"slack_user#user",
		// The relationship-set hash sentinel (slack_channel:C3#relhash@string:
		// <sha256>) rides a relation like any other, so it must be claimed too:
		// an unclaimed relation is writable by anything, and it is exactly the
		// claim that keeps some other writer from forging the sentinel and
		// making the sync engine's skip-if-unchanged short-circuit trust a hash
		// nobody but this source could have produced.
		"slack_channel#relhash",
		"slack_workspace#relhash",
		"slack_usergroup#relhash",
		// The display-name label (slack_channel:C3#label@string:<base64url of
		// the channel name>). Claimed for exactly the sentinel's reason and
		// with one extra edge to it: a label is what the admin console SHOWS a
		// human, so an unclaimed one would let any other in-tree writer decide
		// what a synced channel is called on the page an operator uses to
		// decide whether a sync is doing the right thing. Unlike the two
		// speculative #relhash claims above, this one has a writer today —
		// relsync_kind.go's FetchScope emits it as ordinary scope content.
		"slack_channel#label",
	},
}

func init() {
	relsource.Register(DirectorySyncSource)
}
