//go:build e2e

// This file drives the REAL relwrites.Run, over the REAL slot-bound checker
// (relwrites.NewSlotBoundChecker), against the harness's REAL SpiceDB — the
// gate pkg/tools/toolspec/reviewbot_pr_relwrites_test.go stops one layer
// short of: that file proves the CEL evaluates to the right tuples, never
// once calling relwrites.Run itself. A total refusal of the gate (Run always
// erroring, or the checker never being consulted at all) would still leave
// every assertion in that file green, because none of them go through Run.
//
// The writesRelationships blocks under test are NOT hand-built in Go: they
// are read back from the fixture SpiceboxToolspec CR
// (test/e2e/testdata/agent-pr-identity-e2e/02-toolspec.yaml) and converted the
// same way a real sandbox dispatch converts them — SpiceboxToolspecSpec.ToSpec()
// then relwrites.BlocksFromSpec, the one production translation
// pkg/agent/tool/sandbox.SandboxTool.evaluateWritesRelationships itself calls
// (see blocksFromToolspec below). That is what makes "unmark the block in the
// fixture" (mutation (b), see the package's own mutation-testing notes) a
// one-line edit to the YAML rather than a second, parallel copy of the blocks
// living only in this file — and what makes a field silently dropped from
// that shared translation redden here AND in the sandbox package's own tests.
//
// The fixture's AgentClass declares an authz.slots entry on
// github_pull_request/pin — an ordinary, non-memory permission, composed by
// ComposeSlots into `relation slot_grant_pin: agentsession with expiration`
// plus `permission pin = slot_grant_pin->interact` (github_pull_request has
// no `owner` relation, so the `+ owner` leg is omitted). That relation is
// what NewSlotBoundChecker's ListSlotGrants reads back, and it exists only
// because the fixture's toolBundle reaches toolkit "gh" — a slot naming a
// type no reachable toolkit/MCPServer fragment declares standing for is
// refused outright (pkg/controllers/agentclass/slot_standing.go).
package resourcepool_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	spicedbv1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/relwrites"
	"github.com/authzed/openagentprimitives/test/e2e"
)

const (
	// prIdentityFixtureClass is this file's own AgentClass (a sibling
	// fixture directory to agent-dossier-pool-e2e, not a variant of it: the
	// PR-identity write needs a toolBundle reaching toolkit "gh", which the
	// dossier fixture never declares).
	prIdentityFixtureClass = "pr-identity-e2e"

	// prIdentityToolspec names the fixture SpiceboxToolspec whose
	// writesRelationships blocks this file drives through the real Run —
	// never a second, hand-typed copy of the same CEL.
	prIdentityToolspec = "pr-identity-gh-review"

	// prIdentitySlotPermission is deliberately NOT view_memory/write_memory:
	// this slot binds one pull request instance to a session, the same shape
	// as agent-dossier-pool-e2e's `read` slot on dossier, and names no memory
	// permission at all.
	prIdentitySlotPermission = "pin"

	// GraphQL node ids, one per subtest so no subtest's SpiceDB state can
	// leak into another's assertions.
	prIdentityGrantedPRID    = "PR_kwDOidentitygranted"
	prIdentityNoGrantPRID    = "PR_kwDOidentitynogrant"
	prIdentityNilCheckerPRID = "PR_kwDOidentitynilcheck"
	prIdentityCrossRepoPRID  = "PR_kwDOidentitycrossrepo"

	prIdentityRepoID     = "R_kgDOidentityrepobase"
	prIdentityForkRepoID = "R_kgDOidentityrepofork"

	prIdentityAuthorAcct = "U_kgDOidentityauthoracct"
	prIdentityAdminAcct  = "U_kgDOidentityadminacct"

	// One session per subtest, again so no subtest's slot grant (or lack of
	// one) can be mistaken for another's.
	prIdentitySessionGranted    = "pr-identity-granted-session"
	prIdentitySessionNoGrant    = "pr-identity-no-grant-session"
	prIdentitySessionNilChecker = "pr-identity-nil-checker-session"
	prIdentitySessionCrossRepo  = "pr-identity-cross-repo-session"
)

// TestE2E_PRIdentityWrite_PassesTheSlotGateOrWritesNothing drives the real
// chain end to end: the fixture toolspec's writesRelationships blocks →
// relwrites.Run → relwrites.NewSlotBoundChecker → the harness's real SpiceDB
// → (subtest 1 only) the production PoolDestination audience derivation.
//
// Each subtest is independent — its own session, its own pull request id —
// so ordering never matters and a later subtest's SpiceDB state can never be
// mistaken for an earlier one's:
//
//   - "a slot grant lets both tuples write": the positive. written=2 comes
//     from Run's return, never inferred, and the identity bridge this
//     subtest seeds makes view_memory resolve through the SAME tuples Run
//     just wrote — proving the gate's output is the production input to a
//     real authorization decision, not a value nobody downstream reads.
//   - "no grant: the gate refuses": the negative that makes the positive
//     mean something. The refusal is asserted by TEXT ("no slot-bound
//     grant"), distinguishing it from the expression simply declining to
//     fire (which returns (nil, nil) with no error at all).
//   - "checker unwired: refused by the Task 1 message": the fail-closed
//     default. A harness that forgot to wire SetSlotBoundChecker must not
//     silently write through — it must refuse, loudly, by name.
//   - "cross-repository: author writes, repo is skipped, not refused": the
//     shape distinction between a block that never fires (no error) and one
//     that fires and is refused (an error) — the repo block's own When
//     clause is what skips it here, not the slot gate.
func TestE2E_PRIdentityWrite_PassesTheSlotGateOrWritesNothing(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-pr-identity-e2e"),
		DefaultTimeout: 60 * time.Second,
	})
	// The in-process harness deliberately does not wire the SpiceboxToolspec
	// controller (test/e2e/harness.go's startManager doc comment — the
	// in-process runner factory never dispatches a sandbox tool, so no
	// existing fixture needed it). This scenario's AgentClass reaches Valid
	// only through validateBundles' toolspec-coverage check, which requires
	// Valid=True on the referenced SpiceboxToolspec — so it is stamped here
	// directly, the same way test/e2e/scenarios/sandbox's fixtures do.
	stampToolspecValid(t, h.K8s, prIdentityToolspec)
	h.WaitForAgentClassValid(prIdentityFixtureClass, 60*time.Second)
	// Waited on explicitly: the guardian composes on its OWN reconcile, which
	// AgentClass Valid=True does not imply, and every slot grant below names a
	// relation that does not exist until it has run.
	h.WaitForComposedSchema(60*time.Second, []e2e.SchemaRel{
		{Definition: prType, Relation: authz.SlotGrantRelationName(prIdentitySlotPermission)},
		{Definition: prType, Relation: "author"},
		{Definition: prType, Relation: "repo"},
		{Definition: "github_repo", Relation: "admin"},
	})

	ctx := context.Background()
	ns := "default"
	blocks := blocksFromToolspec(t, h, prIdentityToolspec)
	require.Len(t, blocks, 2, "the fixture toolspec must carry exactly the author and repo writesRelationships blocks")
	w := &relwrites.SpiceDBWriter{Client: h.SpiceDB.Writer(relwrites.Source)}

	t.Run("a slot grant lets both tuples write, and view_memory resolves through them", func(t *testing.T) {
		grantSlot(t, h, prType, prIdentityGrantedPRID, prIdentitySlotPermission, ns, prIdentitySessionGranted)
		checker := relwrites.NewSlotBoundChecker(h.SpiceDB, ns, prIdentitySessionGranted)
		vars := prViewResultVars(ns, prIdentitySessionGranted, prIdentityGrantedPRID, prIdentityAuthorAcct, false, prIdentityRepoID)

		written, err := relwrites.Run(ctx, w, blocks, vars, checker, prIdentityLogf(t))
		require.NoError(t, err, "a session holding the slot grant must not be refused")
		require.Len(t, written, 2, "written must report BOTH tuples, from Run's return — never inferred")

		byRelation := map[string]relwrites.ResolvedTuple{}
		for _, tup := range written {
			byRelation[tup.Relation] = tup
		}
		require.Contains(t, byRelation, "author")
		require.Contains(t, byRelation, "repo")
		assert.Equal(t, prType+":"+prIdentityGrantedPRID, byRelation["author"].Resource)
		assert.Equal(t, "github_user:"+prIdentityAuthorAcct, byRelation["author"].Subject)
		assert.Equal(t, prType+":"+prIdentityGrantedPRID, byRelation["repo"].Resource)
		assert.Equal(t, "github_repo:"+prIdentityRepoID, byRelation["repo"].Subject)

		// The identity bridge view_memory needs to resolve THROUGH the tuples
		// Run just wrote: author->sole_user, and repo->can_admin as the arm
		// that still works when the author arrow does not (not exercised on
		// this PR, but seeded so the union is demonstrably live on both arms).
		authorID := e2e.CanonicalForFakeEmail("pr-identity-author@example.test")
		adminID := e2e.CanonicalForFakeEmail("pr-identity-admin@example.test")
		require.NoError(t, h.SpiceDB.TouchAttestedIdentity(ctx, "github_user", prIdentityAuthorAcct, authorID))
		require.NoError(t, h.SpiceDB.TouchSoleIdentity(ctx, "github_user", prIdentityAuthorAcct, authorID))
		writeRepoRole(t, h, prIdentityRepoID, "admin", prIdentityAdminAcct)
		require.NoError(t, h.SpiceDB.TouchAttestedIdentity(ctx, "github_user", prIdentityAdminAcct, adminID))
		require.NoError(t, h.SpiceDB.TouchSoleIdentity(ctx, "github_user", prIdentityAdminAcct, adminID))

		got := poolAudience(t, h, prType, prIdentityGrantedPRID)
		assert.Contains(t, got, authorID.String(),
			"the author tuple Run just wrote must resolve through author->sole_user; audience: %v", got)
		assert.Contains(t, got, adminID.String(),
			"the repo tuple Run just wrote must resolve through repo->can_admin; audience: %v", got)
	})

	t.Run("no grant: the gate refuses, distinguishably from the expression declining to fire", func(t *testing.T) {
		checker := relwrites.NewSlotBoundChecker(h.SpiceDB, ns, prIdentitySessionNoGrant)
		vars := prViewResultVars(ns, prIdentitySessionNoGrant, prIdentityNoGrantPRID, prIdentityAuthorAcct, false, prIdentityRepoID)

		written, err := relwrites.Run(ctx, w, blocks, vars, checker, prIdentityLogf(t))
		require.Error(t, err, "a session holding no slot grant must be refused, not silently skipped")
		assert.True(t, errors.Is(err, relwrites.ErrSlotBoundRefused),
			"the refusal must carry the gate's sentinel: %v", err)

		// Per-block, not "somewhere in the joined text": Run joins every
		// block's error into one, so asserting the phrase appears ANYWHERE
		// is satisfiable by ONE block reaching the gate while the OTHER
		// failed earlier, inside Evaluate, for an unrelated reason — the
		// exact shape mutation (b) below demonstrates. Naming each block's
		// index is what makes "every block reached the gate" the claim,
		// not "some block did".
		resource := prType + ":" + prIdentityNoGrantPRID
		for i := range blocks {
			assert.Contains(t, err.Error(),
				fmt.Sprintf("block %d: resource %q has no slot-bound grant for this session", i, resource),
				"block %d must reach the slot-bound gate and be refused BY IT, naming this resource", i)
			assert.NotContains(t, err.Error(), fmt.Sprintf("block %d: evaluate:", i),
				"block %d must not fail inside Evaluate — that would mean it never reached the gate "+
					"this subtest is about, and would make the assertion above true for the wrong reason", i)
		}
		assert.Empty(t, written, "nothing may be written from a block the gate refused")

		assert.Empty(t, poolAudience(t, h, prType, prIdentityNoGrantPRID),
			"nothing landed in SpiceDB either — the audience must resolve to nobody")
	})

	t.Run("checker unwired: refused by the Task 1 message, the fail-closed wiring-gap case", func(t *testing.T) {
		// relwrites.NewSlotBoundChecker(nil, ...) is the production shape of an
		// unwired checker (e.g. a nil SpiceDB client at startup), not a
		// hand-substituted literal nil. It is safe from the typed-nil-interface
		// hazard AGENTS.md warns about (see the "Nil interfaces" rule and
		// test/e2e/inprocess_runner_factory.go's own comment on it): the
		// argument here is the untyped constant `nil` passed directly into the
		// SlotGrantLister-typed parameter, never a nil *spicedb.Client variable
		// assigned to an interface first, so `lister == nil` inside the
		// constructor is comparing a TRUE nil interface, not a typed-nil one —
		// and NewSlotBoundChecker returns a true nil SlotBoundChecker in turn.
		checker := relwrites.NewSlotBoundChecker(nil, ns, prIdentitySessionNilChecker)
		vars := prViewResultVars(ns, prIdentitySessionNilChecker, prIdentityNilCheckerPRID, prIdentityAuthorAcct, false, prIdentityRepoID)

		written, err := relwrites.Run(ctx, w, blocks, vars, checker, prIdentityLogf(t))
		require.Error(t, err, "a marked block with no checker wired must refuse, not write through")
		assert.True(t, errors.Is(err, relwrites.ErrSlotBoundRefused))

		// Per-block and the FULL phrase, not "contains unwired somewhere":
		// the same joined-error hazard as the no-grant subtest above applies
		// here too, and a message reworded to something weaker than this
		// exact sentence must redden this assertion.
		for i := range blocks {
			assert.Contains(t, err.Error(),
				fmt.Sprintf("block %d: requires a slot-bound check but the checker is unwired", i),
				"block %d must be refused by name as unwired, not read as an ordinary permission refusal", i)
			assert.NotContains(t, err.Error(), fmt.Sprintf("block %d: evaluate:", i),
				"block %d must not fail inside Evaluate before ever reaching the nil-checker path", i)
		}
		assert.Empty(t, written)

		assert.Empty(t, poolAudience(t, h, prType, prIdentityNilCheckerPRID),
			"nothing landed in SpiceDB either")
	})

	t.Run("cross-repository: the author tuple writes, the repo tuple is skipped by its own When clause", func(t *testing.T) {
		grantSlot(t, h, prType, prIdentityCrossRepoPRID, prIdentitySlotPermission, ns, prIdentitySessionCrossRepo)
		checker := relwrites.NewSlotBoundChecker(h.SpiceDB, ns, prIdentitySessionCrossRepo)
		vars := prViewResultVars(ns, prIdentitySessionCrossRepo, prIdentityCrossRepoPRID, prIdentityAuthorAcct, true, prIdentityForkRepoID)

		written, err := relwrites.Run(ctx, w, blocks, vars, checker, prIdentityLogf(t))
		require.NoError(t, err,
			"the repo block's own When clause must skip it silently (nil, nil) on a fork PR — "+
				"that is not a gate refusal, and must not surface as one")
		require.Len(t, written, 1, "exactly the author tuple must be written")
		assert.Equal(t, prType+":"+prIdentityCrossRepoPRID, written[0].Resource)
		assert.Equal(t, "author", written[0].Relation)
		assert.Equal(t, "github_user:"+prIdentityAuthorAcct, written[0].Subject)

		// Observed twice: Run's return (above) is the mechanism's own answer;
		// reading SpiceDB back independently confirms the write actually
		// landed rather than merely being reported. checkRelation, not
		// lookupSubjects: the package's lookupSubjects helper (used by the
		// sibling PR-preference test) goes through (*spicedb.Client).LookupSubjects,
		// which hardcodes SubjectObjectType "user" — it can see who reaches a
		// PERMISSION transitively, but the author tuple's subject is
		// github_user, not user, so that helper would read empty here whether
		// or not the tuple exists.
		assert.True(t, checkRelation(t, h, prType, prIdentityCrossRepoPRID, "author", "github_user", prIdentityAuthorAcct),
			"the author tuple Run reported written must actually be readable back from SpiceDB")
		assert.False(t, checkRelation(t, h, prType, prIdentityCrossRepoPRID, "repo", "github_repo", prIdentityForkRepoID),
			"the repo relation must never have been written for a cross-repository (fork) pull request")
	})
}

// checkRelation asks SpiceDB directly whether subjectType:subjectID holds
// relation on resourceType:resourceID, FullyConsistent. Used where the
// subject's type is not "user" — lookupSubjects (used elsewhere in this
// package) is a thin wrapper over (*spicedb.Client).LookupSubjects, which
// hardcodes SubjectObjectType "user" and so cannot see a github_user-typed
// subject directly on a raw relation like github_pull_request#author.
func checkRelation(t *testing.T, h *e2e.Harness, resourceType, resourceID, relation, subjectType, subjectID string) bool {
	t.Helper()
	resp, err := h.SpiceDB.CheckPermission(context.Background(), &spicedbv1.CheckPermissionRequest{
		Resource:   &spicedbv1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID},
		Permission: relation,
		Subject: &spicedbv1.SubjectReference{
			Object: &spicedbv1.ObjectReference{ObjectType: subjectType, ObjectId: subjectID},
		},
		Consistency: &spicedbv1.Consistency{Requirement: &spicedbv1.Consistency_FullyConsistent{FullyConsistent: true}},
	})
	require.NoError(t, err, "CheckPermission %s:%s#%s@%s:%s", resourceType, resourceID, relation, subjectType, subjectID)
	return resp.Permissionship == spicedbv1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION
}

// prViewResultVars builds the relwrites CEL bindings a real `gh pr view
// --json id,author,isCrossRepository,headRepository` result produces —
// the exact fields ghPRAuthorWhenCEL/ghPRRepoWhenCEL (and their fixture-YAML
// mirrors) gate on.
func prViewResultVars(ns, session, prID, authorAcct string, crossRepository bool, headRepoID string) map[string]any {
	return map[string]any{
		"args": map[string]any{"argv": []any{"pr", "view", prID}},
		"result": map[string]any{
			"success": true,
			"stdoutJSON": map[string]any{
				"id":                prID,
				"author":            map[string]any{"id": authorAcct},
				"isCrossRepository": crossRepository,
				"headRepository":    map[string]any{"id": headRepoID},
			},
		},
		"session": ns + "/" + session,
	}
}

// blocksFromToolspec reads the named fixture SpiceboxToolspec back out of the
// harness's own K8s client and converts its writesRelationships to
// []relwrites.Block through the SAME two production steps a real sandbox
// dispatch takes: SpiceboxToolspecSpec.ToSpec() (a JSON round-trip into
// pkg/tools/toolspec/spec.Spec — the type a SandboxTool actually carries) and
// relwrites.BlocksFromSpec (the one field-for-field translation
// pkg/agent/tool/sandbox.SandboxTool.evaluateWritesRelationships itself calls).
// No hand-typed copy of that translation lives in this file: a field added to
// spec.RelationshipWriteSpec and forgotten in BlocksFromSpec reddens both the
// sandbox package's own tests (sandbox_tool_slotbound_test.go) and this
// scenario, from the one place it was forgotten.
//
// Reading the blocks back from the applied CR — rather than hand-typing a
// second copy of the CEL in this file — is what makes mutation (b) (unmark
// the block: requireSlotBound: false in the fixture YAML) a one-line fixture
// edit that this test observes, instead of a change nobody here would ever
// see.
func blocksFromToolspec(t *testing.T, h *e2e.Harness, name string) []relwrites.Block {
	t.Helper()
	var ts spiceboxv1alpha1.SpiceboxToolspec
	require.NoError(t, h.K8s.Get(context.Background(), client.ObjectKey{Name: name}, &ts),
		"get SpiceboxToolspec/%s", name)

	resolved, err := ts.Spec.ToSpec()
	require.NoError(t, err, "SpiceboxToolspecSpec.ToSpec() for %s", name)

	return relwrites.BlocksFromSpec(resolved.WritesRelationships)
}

// stampToolspecValid sets Status.Conditions[Valid]=True on the named
// SpiceboxToolspec so the AgentClass controller's toolBundle
// coverage check (validateBundles) accepts it — the harness's in-process
// manager never runs the real SpiceboxToolspec controller (see the Start
// call site above), so nothing else will ever set this condition. Mirrors
// test/e2e/scenarios/sandbox/p1_defaultenv_reaches_toolcall's
// stampToolspecsValid.
func stampToolspecValid(t *testing.T, c client.Client, name string) {
	t.Helper()
	var ts spiceboxv1alpha1.SpiceboxToolspec
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: name}, &ts), "get toolspec %q", name)
	ts.Status.Conditions = []metav1.Condition{{
		Type:               spiceboxv1alpha1.SpiceboxToolspecConditionValid,
		Status:             metav1.ConditionTrue,
		Reason:             "Resolved",
		LastTransitionTime: metav1.Now(),
	}}
	require.NoError(t, c.Status().Update(context.Background(), &ts), "stamp toolspec %q Valid=True", name)
}

// prIdentityLogf adapts relwrites.Run's logFn to t.Logf, so a refusal's
// structured context (block index, resource, error) is visible in test
// output without asserting on log lines.
func prIdentityLogf(t *testing.T) func(msg string, kv ...any) {
	t.Helper()
	return func(msg string, kv ...any) {
		t.Logf("%s %v", msg, kv)
	}
}
