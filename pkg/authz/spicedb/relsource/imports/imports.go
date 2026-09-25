// Package imports blank-imports every in-tree package that registers a
// relsource.Source carrying real claims, then marks the claim table
// complete — turning relsource.CheckWrite / CheckDeleteFilter from a
// fail-closed refusal into the working guard.
//
// Claims register from init() in each writer's own package, so the
// registry's contents depend on which packages a given binary links — not
// on which relations exist. Every binary that can construct a
// relsource-guarded RelWriter (pkg/authz/spicedb.NewWriter /
// (*Client).Writer) must blank-import this package exactly once, in its
// main (or, for the e2e harness, its own top-level wiring), so the claim
// table it consults is the same complete table regardless of which other
// packages that binary happens to pull in. Forgetting to link it is not
// silent: relsource.CheckWrite/CheckDeleteFilter refuse every call until
// MarkComplete has run — see relsource.MarkComplete.
//
// This list is derived from the tree (grep for relsource.Register, filtered
// to sources whose Claims are non-empty), not transcribed from a design
// doc — a package that registers a Source with no Claims (relwrites,
// spicedbbootstrap, guardiangrants, channelsdgrants, memoryshare,
// sessioncapture, e2eharness, ...) contributes nothing to the ownership
// index either way, so it has no reason to be blank-imported here; each of
// those is already imported directly by whatever binary constructs its
// writer.
//
// relsource's own tests, and pkg/authz/spicedb's, cannot import this
// package — it imports pkg/authz/spicedb, so either would be an import
// cycle back through themselves — and call relsource.MarkComplete directly
// instead.
package imports

import (
	_ "github.com/authzed/openagentprimitives/pkg/authz/guardian/approval"           // LeakageSource: infoleakage_grant#session, infoleakage_grant#audience_subject
	_ "github.com/authzed/openagentprimitives/pkg/authz/spicedb"                     // TypedWritesSource: agentsession#parent, agentsession#child, github_user#user, github_user#sole_user
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"      // DirectorySyncSource: github_org#member/#relhash, github_team#org/#direct_member/#subteam/#relhash, github_repo#org/#admin/#maintain/#write/#triage/#reader/#relhash, github_repo_url#repo
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/onepassword" // DirectorySyncSource: onepassword_group#member/#relhash
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"       // DirectorySyncSource: slack_channel#member/#workspace, slack_workspace#member, slack_usergroup#member, slack_user#user
	_ "github.com/authzed/openagentprimitives/pkg/memory/pttagmint"                  // Source: pt_tag#session/#direct_reader/#derived_from/#untrusted_origin
	_ "github.com/authzed/openagentprimitives/pkg/memory/spicedbauthorizer"          // Source: memory_entry#session, memory_entry#creator

	"github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource"
)

func init() {
	relsource.MarkComplete()
}
