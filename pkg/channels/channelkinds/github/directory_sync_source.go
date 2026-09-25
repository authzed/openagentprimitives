package github

import (
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
)

// DirectorySyncSource is the relsource identity this kind writes as. It
// claims every relation the directory sync's FetchScope writes on
// github_org, github_team and github_repo, plus the github_repo_url#repo
// bridge tuple the sync mints to tie a URL an agent typed to the repository
// GitHub enumerated — EXCLUSIVELY: the sync diffs and PRUNES, deleting
// whatever a fetch no longer reports, and that is only correct when nothing
// else writes what it deletes.
//
// Not claiming a relation does NOT protect it, and reading this list that
// way is a mistake this file once made in writing: relsource.CheckWrite
// refuses only what ANOTHER source claims, so an unclaimed relation is one
// every writer may touch. What keeps the sync off github_repo_url#owner is
// that relsync's cross-resource reap deletes only relations the source
// claims — a bound the engine enforces, not an omission here (see
// reapAbsentCrossResourceTuples). This list says what the sync WRITES;
// staying off everything else is the engine's job.
//
// Three relations that share these definitions are deliberately NOT claimed:
//
//   - github_user#sole_user and github_user#user belong to the useridentity
//     reconciler (pkg/authz/spicedb's TypedWritesSource). An attestation
//     proves who you are upstream, never what you may do there — claiming
//     either would let this sync mint identity, not just role membership.
//   - github_repo_url#owner is where a session grant is written, by the
//     approval flow. The sync has no business writing a human's approval,
//     so it claims nothing there.
//   - github_repo#owner is the inert compatibility shim toolkits/gh.yaml
//     documents: nothing writes it, and it exists only so a slot left over
//     from the URL-keyed github_repo era resolves instead of wedging every
//     schema write. Claiming a relation nothing writes would claim nothing
//     real and only invite a future writer to collide with a shim.
//
// SubjectIdentityClaims is EMPTY, and that is a positive declaration rather
// than an omission: not one relation this sync writes takes a bare `user:`
// subject. Every membership it mints names a USERSET —
// github_org:<login>#member@github_user:<id>#sole_user and the same shape for
// every github_repo role (relsync_kind.go's soleUserRoleTuple, the only builder
// for them), github_team:<id>#direct_member@github_team:<id>#member,
// github_team#org / github_repo#org / github_team#subteam / github_repo_url#repo
// naming other objects, and #relhash a string digest.
//
// The user↔account link this sync's tuples hang off — github_user:<id>#user and
// #sole_user, the only bare-user shapes in this definition family — belongs to
// the useridentity reconciler (pkg/authz/spicedb's TypedWritesSource), which
// declares them; see this source's note above on why an attestation is not
// this sync's to write.
var DirectorySyncSource = relsource.Source{
	Name:                  "githubdirectorysync",
	DisplayName:           "GitHub",
	SubjectIdentityClaims: nil,
	Claims: []string{
		"github_org#member",
		"github_org#relhash",
		"github_team#org",
		"github_team#direct_member",
		"github_team#subteam",
		"github_team#relhash",
		"github_repo#org",
		"github_repo#admin",
		"github_repo#maintain",
		"github_repo#write",
		"github_repo#triage",
		"github_repo#reader",
		"github_repo#relhash",
		"github_repo_url#repo",
	},
}

func init() { relsource.Register(DirectorySyncSource) }
