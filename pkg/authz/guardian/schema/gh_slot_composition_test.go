package schema_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
	"github.com/authzed/openagentprimitives/toolkits"
)

// shippedSchema is the base scaffold with every builtin toolkit's fragment
// composed in — the schema a real cluster writes before any slot is applied.
// Both tests below run against it rather than a synthetic fixture, because both
// are claims about the SHIPPED gh type model meeting the SHIPPED slot composer,
// and a hand-written stand-in for either half would keep passing while the real
// one drifted.
func shippedSchema(t *testing.T) string {
	t.Helper()
	out, err := schema.ComposeBase(schema.ToolkitFragments(toolkits.All()))
	require.NoError(t, err)
	return out
}

// definitionBlock, which slices one `definition <name> { ... }` out of a
// composed schema, lives in slots_reserved_scaffold_test.go — same test
// package, so it is reused rather than written twice.

// A KNOWN LIMITATION, pinned here so it cannot be forgotten and cannot be
// "fixed" without someone being told what else to update. This test does not
// describe desirable behavior.
//
// gh.yaml declares `permission read = owner + repo->can_read` — the union that
// lets the GitHub directory sync contribute repository access ADDITIVELY, on top
// of whatever local `owner` grant already worked. composeOneSlot OWNS a slot
// permission's expression line and REPLACES whatever the author wrote with
// `slot_grant_<perm>->interact + owner`. So for any AgentClass that declares a
// slot on that permission, `repo->can_read` is gone and the sync contributes
// nothing through it.
//
// The composer is not wrong to do this in general — replacing the line is what
// stops an AgentClass author widening a slot permission, and what removes a
// wildcard leaf (TestComposeSlots_noWildcardLeafSurvives is the same mechanism
// doing its job). It simply cannot tell a toolkit-authored expression, which
// ships at compile time and is reviewed, from a tenant-authored one.
// `repo->can_read` is a widening; it is just a legitimate one.
//
// **If you are here because this test failed**, you have probably made
// composition provenance-aware, which is the tracked fix ("Slot composition
// erases a toolkit-authored permission expression"). That is good news.
// Update, in this order:
//
//  1. delete this test and replace it with one asserting the arrow SURVIVES;
//  2. remove the KNOWN LIMITATION block above gh.yaml's `permissions:`;
//  3. remove the caveat in docs/relationshipsource.md ("A slot on a synced
//     permission takes the slot's answer, not the directory's");
//  4. revisit whether github_repo_url can move to `standing: required`, which
//     that TODO entry names this as the blocker for.
func TestComposeSlots_ErasesTheGhForgeTraversal(t *testing.T) {
	src := shippedSchema(t)

	// The premise: before any slot, the arrow is there. Without this the test
	// below would pass just as well against a schema that never had one.
	require.Contains(t, definitionBlock(t, src, "github_repo_url"),
		"permission read = owner + repo->can_read",
		"gh.yaml must declare the additive union, or this test pins nothing")

	out, changed, skipped, err := schema.ComposeSlots(src,
		[]schema.SlotPair{{ResourceType: "github_repo_url", Permission: "read"}})
	require.NoError(t, err)
	require.True(t, changed)
	require.Empty(t, skipped,
		"github_repo_url IS declared, so the slot is composed rather than skipped — "+
			"which is precisely why the erasure happens")

	block := definitionBlock(t, out, "github_repo_url")

	assert.Contains(t, block,
		"permission read = "+schema.SlotGrantRelationName("read")+"->interact + owner",
		"the composer owns the line and writes its own expression")
	assert.NotContains(t, block, "repo->can_read",
		"KNOWN LIMITATION: a slot erases the forge traversal on the permission it names. "+
			"If this now passes, read this test's doc comment before deleting it")

	// And the blast radius is exactly the permissions a slot NAMED. The
	// unslotted ones keep their arrows, which is why the limitation is a
	// per-permission caveat rather than "slots disable the sync".
	assert.Contains(t, block, "permission write = owner + repo->can_write",
		"a permission no slot named must be untouched")
	assert.Contains(t, block, "permission admin = owner + repo->can_admin",
		"a permission no slot named must be untouched")
}

// The upgrade case, and the reason github_repo carries an unclaimed `owner`.
//
// Before the URL/forge split, `github_repo` WAS the URL-keyed type with an
// `owner: user` relation, so an AgentClass declaring a slot on github_repo#read
// was the natural thing to write. Those AgentClasses still exist in clusters.
//
// ComposeSlots skips only a type the schema does not DECLARE, and github_repo is
// still declared — as the forge-keyed type — so a stale slot is composed onto it
// and the composer CREATES the permission it does not find. Without an `owner`
// relation the result is `permission read = slot_grant_read->interact + owner`
// referencing nothing, and the pass that would catch that in RunAll —
// ValidateComposedSchema, in-process — refuses the ENTIRE composed schema
// before it ever reaches a live SpiceDB, cluster-wide, for every
// AgentSessionGrants at once, not only for the AgentClass at fault.
//
// The shim makes it resolve. It grants nothing: nothing writes an owner tuple
// on a forge-keyed repository, and a slot carried over from the URL-keyed era
// holds URL-derived resource ids, which match no forge-keyed object id.
func TestComposeSlots_AStaleGithubRepoSlotResolvesInsteadOfWedgingTheSchema(t *testing.T) {
	out, changed, skipped, err := schema.ComposeSlots(shippedSchema(t),
		[]schema.SlotPair{{ResourceType: "github_repo", Permission: "read"}})
	require.NoError(t, err)
	require.True(t, changed)
	require.Empty(t, skipped,
		"github_repo is still declared, so the stale slot is NOT skipped — it is composed, "+
			"which is the whole hazard this shim answers")

	unresolved, err := schema.UnresolvedReferences(out)
	require.NoError(t, err)
	assert.Empty(t, unresolved,
		"a stale github_repo slot must compose to a schema whose references all resolve; "+
			"a dangling one is refused by WriteSchema for the whole cluster at once")

	// Prove the shim is what carries it, rather than some unrelated property of
	// the composed text: drop the relation and the same slot dangles. Matched
	// by trimmed content rather than a literal indented string — shippedSchema
	// now round-trips through composeFragmentSet's compiler, which renders its
	// own (tab) indentation rather than preserving gh.yaml's source whitespace
	// verbatim; the indentation is cosmetic, the relation's presence is not.
	//
	// Scoped to the github_repo BLOCK rather than removed from the whole
	// composed text: "relation owner: user" appears five times in the shipped
	// schema (docker_daemon, git_repo, github_repo_url, k8s_namespace,
	// github_repo — github_repo's is the fifth), and removeTrimmedLine takes
	// the FIRST match wherever it is. An unscoped removal here silently strips
	// docker_daemon's owner relation instead, and UnresolvedReferences would
	// still report something (a dangling reference on docker_daemon), so a
	// bare assert.NotEmpty would pass without ever exercising github_repo's
	// shim at all.
	block := definitionBlock(t, out, "github_repo")
	withoutBlock := removeTrimmedLine(t, block, "relation owner: user")
	require.NotEqual(t, block, withoutBlock, "the shim relation must be present in the github_repo block to remove")
	without := strings.Replace(out, block, withoutBlock, 1)
	require.NotEqual(t, out, without, "the shim relation must be present to remove")

	degraded, err := schema.UnresolvedReferences(without)
	require.NoError(t, err)
	require.NotEmpty(t, degraded,
		"without the shim the stale slot must dangle, or this test proves nothing about it")
	assert.Equal(t, "github_repo", degraded[0].Definition,
		"the dangling reference must be reported against github_repo — the resource the stale slot "+
			"targets — not some other definition whose 'relation owner: user' line happened to match first")
}
