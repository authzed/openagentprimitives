//go:build e2e

// This file is the AUDIENCE half of resource-scoped memory, for one resource
// type: a GitHub pull request. Its siblings (pool_read_test.go,
// pool_write_test.go) prove how a SESSION reaches a pool — a slot grant, and
// nothing else. This one proves the separate question the slot deliberately
// does not answer: who may SEE what a pool holds.
//
// The two are different permissions on purpose (memory.PermissionViewMemory's
// own doc says so), and `github_pull_request` is where the distinction first
// carries real authority: toolkits/gh.yaml declares
//
//	permission view_memory = author->sole_user + repo->can_admin
//
// and nothing until now proved that expression resolves against a real SpiceDB
// holding real tuples, as opposed to reading as though it would.
//
// WHY THE OBSERVABLE IS THE AUDIENCE DERIVATION AND NOT A READ. There is no
// user-facing read of a pool to drive: a pool's entries are fetched by a
// SESSION, gated on its slot grant, and the per-entry filter behind that
// (memory_entry#read = session->read_transcript) never consults view_memory at
// all. view_memory is expanded LIVE, by LookupSubjects, at the two places the
// runner needs to know a pool's readers — the info-leakage gate that decides
// whether this session may write into the pool (hooks.PoolDestination) and the
// per-datum tag minted over a pool read (hooks/resultreads.go). Both ask
// SpiceDB the same question through the same closure internal/cmd/runner
// wires, so this file asks it as a PRODUCTION OBJECT (hooks.PoolDestination)
// rather than hand-spelling "#view_memory" at the call site. A test that spelled
// the permission itself would keep passing if the gate started expanding the
// slot permission instead — which is precisely the substitution
// PermissionViewMemory's doc warns about.
//
// NOTHING HERE DECLARES THE PR TYPE, and nothing should. The embedded gh
// toolkit is a compile-time schema fragment that the guardian folds into EVERY
// compose (agentsessiongrants_controller.go's toolkitFrags → schemaBaseline),
// so the definition under test is the shipped one, not a fixture copy. A copy
// would also be a second `definition github_pull_request` in one composed
// schema — the duplicate-declaration failure the fragment's own comment exists
// to warn about.
package resourcepool_test

import (
	"context"
	"testing"
	"time"

	spicedbv1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/authz/relwrites"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/test/e2e"
)

const (
	// prType is contributed by toolkits/gh.yaml, not by this package's fixture.
	prType = "github_pull_request"

	// GraphQL node ids — the shape `gh pr view --json id` returns and the shape
	// the pinned CEL writes (pkg/tools/toolspec's ghPRAuthorResourceCEL). TWO
	// pull requests, because "a repo admin reaches it when the author arrow
	// does not resolve" has to be a PR with no author tuple AT ALL; asserting
	// it on a PR whose author was deleted by an earlier subtest would prove
	// only that the delete worked.
	prAuthored = "PR_kwDOaudienceauthored"
	prNoAuthor = "PR_kwDOaudienceadminonly"

	// One repository, shared by both PRs, carrying two different collaborator
	// roles so the audience's `can_admin` arm is demonstrably about ADMIN and
	// not about "has any role here".
	ghRepoID = "R_kgDOaudiencerepo"

	ghAuthorAcct = "U_kgDOauthor"
	ghAdminAcct  = "U_kgDOadmin"
	ghReaderAcct = "U_kgDOreader"

	// The observation the audience governs. Unique, like every text in this
	// package, so an assertion names the entry it means.
	prObservationText = "pr-preference-observation-recorded-on-the-pull-request"
)

// TestE2E_PRPreference_AudienceIsAuthorAndRepoAdmins drives the whole chain:
// the shipped gh toolkit fragment → the guardian's compose into live SpiceDB →
// the identity bridge's attested edge → the audience the runner's info-leakage
// gate derives for a pool write.
//
// Each subtest is one claim and each can fail on its own:
//
//   - "the shipped type composed": the schema half. Reddens if the toolkit
//     fragment stopped reaching the composed schema, or if view_memory ever
//     became a composer-owned slot line — which would silently redefine who may
//     read every observation already on every PR.
//   - "the author is in the audience": the `author->sole_user` arm.
//   - "a repo admin reaches it when the author arrow does not": the
//     `repo->can_admin` arm, on a PR that HAS no author, so the arm is
//     load-bearing rather than merely present.
//   - "a stranger is not": the negative that makes the positives mean
//     something. The stranger holds a real, live `reader` role on the same
//     repository, so their absence is attributable to `can_admin` and not to a
//     dead identity binding — and the subtest anchors on the audience still
//     holding BOTH its members, so an audience that collapsed could not read as
//     a working gate.
//   - "sole_user withdrawn": the behaviour the design spec says to STATE rather
//     than discover. It proves both halves — the author's access is gone and
//     the admin's is not — so "withdrawn for this one account" is
//     distinguishable from "sole_user stopped resolving at all".
func TestE2E_PRPreference_AudienceIsAuthorAndRepoAdmins(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-dossier-pool-e2e"),
		DefaultTimeout: 60 * time.Second,
	})
	// The fixture's MCPServer pins one tool and the controller probes the live
	// server before reporting Valid; nothing here ever calls it. See
	// pool_read_test.go for the failure mode this avoids.
	h.MCP.OnTool("get_dossier", func(_ map[string]any) any { return map[string]any{"id": "d"} })
	// The fixture is reused unchanged, and only for its AgentClass: the
	// guardian composes when it has an AgentSessionGrants to reconcile, and the
	// composed schema is what carries the embedded toolkit fragments. Nothing
	// in this file touches a dossier.
	h.WaitForAgentClassValid(fixtureClass, 60*time.Second)
	// Waited on explicitly for the same reason the sibling scenarios wait on
	// their slot relations: compose happens on the guardian's OWN reconcile,
	// which AgentClass Valid=True does not imply, and every tuple below names a
	// relation that does not exist until it has run.
	h.WaitForComposedSchema(60*time.Second, []e2e.SchemaRel{
		{Definition: prType, Relation: "author"},
		{Definition: prType, Relation: "repo"},
		{Definition: "github_repo", Relation: "admin"},
	})

	ctx := context.Background()
	authorID := e2e.CanonicalForFakeEmail("pr-author@example.test")
	adminID := e2e.CanonicalForFakeEmail("repo-admin@example.test")
	readerID := e2e.CanonicalForFakeEmail("repo-reader@example.test")

	// The observation the audience governs. Seeded straight through the
	// harness's facade, exactly as the sibling scenarios seed theirs: the WRITE
	// path is not what this file is about, and requiring the Put to succeed is
	// what makes "an observation recorded on the PR" a stored entry in the PR's
	// own pool rather than a notion. It also pins the thing a GraphQL node id
	// has to satisfy to be a pool at all — memory.ResourceScope refuses a ':'
	// or a '/' in either half.
	prPool := resourceScope(t, prType, prAuthored)
	seedObservation(t, h, prPool, "obs-pr-preference", prObservationText)

	// The relationship graph, written through the SOURCES that own each
	// relation rather than through the harness's unclaimed one, so a tuple this
	// test seeds is labelled with the writer it stands in for:
	//
	//   github_pull_request#author/#repo  relwrites.Source — the CEL block on
	//       reviewbot's gh toolspec writes these after `gh pr view` (the
	//       expressions are pinned in pkg/tools/toolspec).
	//   github_repo#admin/#reader         github.DirectorySyncSource — the
	//       forge directory sync writes collaborator roles.
	//   github_user#user/#sole_user       (*spicedb.Client)'s typed writes, the
	//       SAME calls pkg/controllers/useridentity makes. Used rather than a
	//       raw tuple because the withdrawal subtest below needs the production
	//       DeleteSoleIdentity, and a fixture that wrote the edge by hand and
	//       withdrew it by API would be testing two different things.
	writePRRel(t, h, prAuthored, "author", "github_user", ghAuthorAcct)
	writePRRel(t, h, prAuthored, "repo", "github_repo", ghRepoID)
	// prNoAuthor deliberately gets NO author tuple.
	writePRRel(t, h, prNoAuthor, "repo", "github_repo", ghRepoID)

	writeRepoRole(t, h, ghRepoID, "admin", ghAdminAcct)
	writeRepoRole(t, h, ghRepoID, "reader", ghReaderAcct)

	// BOTH edges for every account, which is what the useridentity reconciler
	// writes for a lone claimant. #user is not decoration here: it is what makes
	// the withdrawal subtest attributable. With only #sole_user present, an
	// audience of `author->user` would resolve to nobody and the FIRST subtest
	// would fail — so the test could not tell an audience reading the attested
	// edge from one reading any edge.
	require.NoError(t, h.SpiceDB.TouchAttestedIdentity(ctx, "github_user", ghAuthorAcct, authorID))
	require.NoError(t, h.SpiceDB.TouchSoleIdentity(ctx, "github_user", ghAuthorAcct, authorID))
	require.NoError(t, h.SpiceDB.TouchAttestedIdentity(ctx, "github_user", ghAdminAcct, adminID))
	require.NoError(t, h.SpiceDB.TouchSoleIdentity(ctx, "github_user", ghAdminAcct, adminID))
	require.NoError(t, h.SpiceDB.TouchAttestedIdentity(ctx, "github_user", ghReaderAcct, readerID))
	require.NoError(t, h.SpiceDB.TouchSoleIdentity(ctx, "github_user", ghReaderAcct, readerID))

	t.Run("the shipped type composed: github_pull_request carries view_memory as an unslotted permission", func(t *testing.T) {
		resp, err := h.SpiceDB.ReadSchema(ctx, &spicedbv1.ReadSchemaRequest{})
		require.NoError(t, err, "ReadSchema")
		schema := resp.SchemaText

		require.Contains(t, schema, "definition "+prType,
			"the embedded gh toolkit's fragment must reach the composed schema; got:\n%s", schema)

		// The composer must NOT own this line. isSlottableMemoryPermission is
		// what refuses a slot on a memory AUDIENCE permission, and this is that
		// refusal's observable consequence in a LIVE cluster schema — with
		// every toolkit fragment, every fixture fragment and slot composition
		// all actually having run. A composed `view_memory = slot_grant_...`
		// would hand the pool's readership to whoever holds a session grant.
		assert.Contains(t, schema, "permission "+memory.PermissionViewMemory+" = author->sole_user + repo->can_admin",
			"the pull request's audience must survive composition verbatim; got:\n%s", schema)
		assert.NotContains(t, schema,
			"permission "+memory.PermissionViewMemory+" = slot_grant_",
			"view_memory must never be composed as a slot; got:\n%s", schema)
	})

	t.Run("the PR author is in the audience of an observation recorded on the PR", func(t *testing.T) {
		got := poolAudience(t, h, prType, prAuthored)
		assert.Contains(t, got, authorID.String(),
			"the author arrow must traverse github_user#sole_user to the platform user; audience: %v", got)
	})

	t.Run("a repo admin reaches it when the author arrow does not resolve", func(t *testing.T) {
		got := poolAudience(t, h, prType, prNoAuthor)
		require.Contains(t, got, adminID.String(),
			"repo->can_admin must reach a PR that has no author tuple at all; audience: %v", got)
		// Per-instance, in the direction the union could hide: the author of
		// the OTHER pull request is not in this one's audience, so the admin
		// arm is not quietly dragging the whole repository's PR authors along.
		assert.NotContains(t, got, authorID.String(),
			"a PR with no author tuple must not inherit another PR's author; audience: %v", got)
	})

	t.Run("a stranger cannot read it: a repo reader holds no view_memory on either PR", func(t *testing.T) {
		// The anchor, and it is two claims at once. The audience must still
		// hold BOTH its members — otherwise an audience that collapsed to
		// nothing would satisfy every absence below and read as a working
		// gate, which is exactly the fail-closed direction an untupled PR
		// already sits in.
		authored := poolAudience(t, h, prType, prAuthored)
		require.Contains(t, authored, authorID.String(), "anchor: the author must be in the audience")
		require.Contains(t, authored, adminID.String(), "anchor: the repo admin must be in the audience")

		// And the stranger is a REAL, bound, currently-authorized person on
		// this very repository — just not an admin. Read raw rather than
		// through the pool derivation because can_read is deliberately not the
		// audience: this asserts the fixture's identity bridge is live for
		// them, so their absence below is about can_admin and not about a
		// binding that never resolved.
		readers := lookupSubjects(t, h, "github_repo:"+ghRepoID+"#can_read")
		require.Contains(t, readers, readerID.String(),
			"anchor: the stranger must hold a live repository role; can_read: %v", readers)

		assert.NotContains(t, authored, readerID.String(),
			"a repo reader is not a repo admin and did not author the PR; audience: %v", authored)
		assert.NotContains(t, poolAudience(t, h, prType, prNoAuthor), readerID.String(),
			"and the same holds for the PR whose only arm is repo->can_admin")
	})

	t.Run("sole_user withdrawn: the author loses it, the admin keeps it", func(t *testing.T) {
		// MUTATES SHARED SPICEDB STATE and therefore runs last. Restored on
		// cleanup so a future subtest added above this one cannot be silently
		// poisoned by the withdrawal.
		require.Contains(t, poolAudience(t, h, prType, prAuthored), authorID.String(),
			"precondition: the author must hold the audience before it is withdrawn")

		// The production withdrawal, by the production call: the useridentity
		// reconciler invokes exactly this the moment a second platform subject
		// claims one GitHub account, because nothing can tell which claim is
		// legitimate.
		require.NoError(t, h.SpiceDB.DeleteSoleIdentity(ctx, "github_user", ghAuthorAcct),
			"withdraw the attested edge")
		t.Cleanup(func() {
			_ = h.SpiceDB.TouchSoleIdentity(ctx, "github_user", ghAuthorAcct, authorID)
		})

		// The NON-attested edge survives, and this is the assertion that makes
		// the loss below attributable. #user is written for every binding and
		// is untouched by a withdrawal; if the author still disappears from the
		// audience, the audience is reading #sole_user and not merely "some
		// edge from this account to this person".
		bound, err := h.SpiceDB.LookupAttestedIdentitySubjects(ctx, "github_user", ghAuthorAcct)
		require.NoError(t, err, "lookup github_user#user after the withdrawal")
		require.Contains(t, bound, authorID.String(),
			"precondition: #user must survive a #sole_user withdrawal, or the loss below proves nothing")

		got := poolAudience(t, h, prType, prAuthored)
		assert.NotContains(t, got, authorID.String(),
			"two claimants on one GitHub account costs the author view_memory, fail-closed; audience: %v", got)
		// The other half, and the reason the union is in the expression at all:
		// the PR stays readable by its repository's admins. It also tells a
		// withdrawal apart from sole_user having stopped resolving — the
		// admin's own edge is the same kind of edge, on a different account.
		assert.Contains(t, got, adminID.String(),
			"repo->can_admin must keep the PR readable when the author arm dies; audience: %v", got)
	})
}

// poolAudience asks the PRODUCTION audience derivation who may see what a
// resource's memory pool holds.
//
// hooks.PoolDestination rather than a raw LookupSubjects on "#view_memory":
// the permission name is what is under test, and a test that spelled it here
// would keep passing if the gate started expanding the session's SLOT
// permission instead — the substitution memory.PermissionViewMemory's doc
// comment exists to warn about. The lookup closure is byte-for-byte the one
// internal/cmd/runner wires onto Loop.SpiceDBLookupSubjects.
func poolAudience(t *testing.T, h *e2e.Harness, objType, objID string) []string {
	t.Helper()
	lookup := func(ctx context.Context, resource, permission string) ([]string, error) {
		return h.SpiceDB.LookupSubjects(ctx, resource+"#"+permission)
	}
	got, err := hooks.PoolDestination(lookup, objType, objID).Audience(context.Background())
	require.NoError(t, err, "audience of %s:%s", objType, objID)
	return got
}

// lookupSubjects expands one subject ref, for the fixture-anchoring reads that
// are deliberately NOT the pool audience (a repository's can_read).
func lookupSubjects(t *testing.T, h *e2e.Harness, ref string) []string {
	t.Helper()
	got, err := h.SpiceDB.LookupSubjects(context.Background(), ref)
	require.NoError(t, err, "LookupSubjects(%s)", ref)
	return got
}

// writePRRel writes one github_pull_request relationship under relwrites.Source
// — the source reviewbot's `gh pr view` writesRelationships block writes under,
// so the tuple carries the label of the writer it stands in for. The CEL that
// produces it in production is pinned in pkg/tools/toolspec; driving a real
// sandbox toolspec from here would test that block twice and this expression
// not at all.
func writePRRel(t *testing.T, h *e2e.Harness, prID, relation, subjectType, subjectID string) {
	t.Helper()
	_, err := h.SpiceDB.Writer(relwrites.Source).WriteRelationships(context.Background(),
		&spicedbv1.WriteRelationshipsRequest{
			Updates: []*spicedbv1.RelationshipUpdate{{
				Operation: spicedbv1.RelationshipUpdate_OPERATION_TOUCH,
				Relationship: &spicedbv1.Relationship{
					Resource: &spicedbv1.ObjectReference{ObjectType: prType, ObjectId: prID},
					Relation: relation,
					Subject: &spicedbv1.SubjectReference{
						Object: &spicedbv1.ObjectReference{ObjectType: subjectType, ObjectId: subjectID},
					},
				},
			}},
		})
	require.NoError(t, err, "write %s:%s#%s@%s:%s", prType, prID, relation, subjectType, subjectID)
}

// writeRepoRole writes one github_repo collaborator role under
// github.DirectorySyncSource, which CLAIMS these relations — the harness's own
// unclaimed source would be refused by relsource.CheckWrite, deliberately, so
// a fixture cannot quietly stand in for a writer whose behaviour it is
// supposed to be exercising.
//
// The subject is `github_user:<acct>#sole_user`, the subject-relation form
// github_repo's `admin` (and every sibling role) declares: repository authority
// traverses the attested edge, never #user.
func writeRepoRole(t *testing.T, h *e2e.Harness, repoID, role, account string) {
	t.Helper()
	_, err := h.SpiceDB.Writer(github.DirectorySyncSource).WriteRelationships(context.Background(),
		&spicedbv1.WriteRelationshipsRequest{
			Updates: []*spicedbv1.RelationshipUpdate{{
				Operation: spicedbv1.RelationshipUpdate_OPERATION_TOUCH,
				Relationship: &spicedbv1.Relationship{
					Resource: &spicedbv1.ObjectReference{ObjectType: "github_repo", ObjectId: repoID},
					Relation: role,
					Subject: &spicedbv1.SubjectReference{
						Object:           &spicedbv1.ObjectReference{ObjectType: "github_user", ObjectId: account},
						OptionalRelation: "sole_user",
					},
				},
			}},
		})
	require.NoError(t, err, "write github_repo:%s#%s@github_user:%s#sole_user", repoID, role, account)
}
