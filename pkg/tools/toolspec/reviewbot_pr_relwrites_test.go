package toolspec

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/relwrites"
)

// ghPRAuthorWhenCEL is a hand-maintained PARALLEL COPY of the `when` clause on
// the writesRelationships block carried by TWO YAML files — the fixture
// SpiceboxToolspec (test/e2e/testdata/agent-pr-identity-e2e/02-toolspec.yaml)
// and the shipped `demo-reviewbot-gh-review` toolspec
// (examples/reviewbot/manifests/toolspecs.yaml). It exists here, as a Go
// constant, so this file's CEL-semantics tests (numeric-id fail-closed,
// missing-field skip, wrong-subcommand) run without the e2e build tag or a
// live SpiceDB — the write path is exercised end to end, over the real
// relwrites.Run and slot-bound checker, by
// test/e2e/scenarios/resourcepool/pr_identity_write_test.go (the fixture) and
// trigger_slot_binding_test.go (the shipped demo's own shape: a slot bound at
// trigger time rather than seeded directly). The shipped demo's class binds
// the slot through its authz.slots entry on github_pull_request/write_memory,
// fillFrom: [trigger] (examples/reviewbot/manifests/agentclass.yaml) — see
// toolkits/gh.yaml's github_pull_request comment for the current state.
//
// All three copies are kept in sync BY HAND. Nothing derives either YAML from
// this constant or the reverse, so an edit to one that is not mirrored to the
// others will not be caught by this test, or by anything else in the suite —
// see the matching comment on each writesRelationships block.
//
// writesRelationships is SPEC-WIDE on a SpiceboxToolspec, not scoped to one
// subcommand — the argv guard (args.argv[0] == "pr" && args.argv[1] ==
// "view") is the only thing that keeps this block from firing on every gh
// call the toolspec allows, which is why
// TestGhPRRelWrites_DoesNotFireOnAnotherSubcommand exists: it proves the
// guard, not the block, does the scoping.
//
// has(result.stdoutJSON.id), alongside has(result.stdoutJSON.author), is
// required for the same reason `--json author` (omitting `id`) is a routine
// model mistake, not an edge case: without this clause the gate would still
// fire, and ghPRAuthorResourceCEL's `result.stdoutJSON.id` reference would
// then fail closed at eval time ("no such key: id") — safe, but a noisy,
// entirely avoidable per-call log entry for the single most likely partial
// `--json` selection. See TestGhPRRelWrites_DoesNotFireWithoutID.
const ghPRAuthorWhenCEL = `size(args.argv) > 1 && args.argv[0] == "pr" && args.argv[1] == "view" && has(result.stdoutJSON) && has(result.stdoutJSON.id) && has(result.stdoutJSON.author)`

// ghPRAuthorResourceCEL and ghPRAuthorSubjectCEL are the same hand-maintained
// PARALLEL COPY as ghPRAuthorWhenCEL above, for the same block's
// tuple.resource and tuple.subject expressions, mirroring both YAML twins.
// All three Go constants are kept in sync with both YAML files BY HAND, and
// nothing derives one from another.
//
// The concatenation is deliberately raw `+`, with no string() cast: CEL has
// no _+_(string, double) overload, so feeding either side a non-string value
// (a numeric id, say) is a hard eval error — fail closed, same outcome
// family as the when clause's has()-gated skip, just via a different
// mechanism. See TestGhPRRelWrites_ResourceFailsClosedOnANumericID: adding a
// string() cast to "fix" that error would silently trade the hard failure
// for a scientific-notation object id that fails SpiceDB's object_id regex
// downstream instead — worse, not better — which is exactly what that test
// exists to catch.
//
// ghPRAuthorSubjectCEL reads result.stdoutJSON.author.id — the AUTHOR's node
// id — never result.stdoutJSON.id (the PR's own id). Nothing else in this
// file structurally distinguishes those two fields; a subject bug here would
// compile and run fine while silently granting read access to the wrong
// account. See TestGhPRRelWrites_SubjectIsTheAuthorID.
const ghPRAuthorResourceCEL = `"github_pull_request:" + result.stdoutJSON.id`
const ghPRAuthorSubjectCEL = `"github_user:" + result.stdoutJSON.author.id`

// evalBool compiles expr against relwrites.CompileBoolExpr — the SAME CEL
// environment (args/result/item/session/call, plus spicedb_user_id) the
// relwrites evaluator itself compiles When/Tuple expressions against — and
// evaluates it over vars. Building on the shared compiler rather than a
// hand-rolled cel.Env means a future change to that environment (a dropped
// function, a renamed variable) fails this test instead of only failing
// silently in production, the same reasoning relwrites.CompileBoolExpr's own
// doc comment gives for observe.Evaluate sharing it.
func evalBool(t *testing.T, expr string, vars map[string]any) bool {
	t.Helper()
	prg, err := relwrites.CompileBoolExpr(expr)
	require.NoError(t, err, "compile %q", expr)
	v, _, err := prg.Eval(vars)
	require.NoError(t, err, "eval %q against %#v", expr, vars)
	b, ok := v.Value().(bool)
	require.True(t, ok, "expected bool, got %T (%v)", v.Value(), v.Value())
	return b
}

// evalString is evalBool's string-typed counterpart, built the same way on
// relwrites.CompileStringExpr.
func evalString(t *testing.T, expr string, vars map[string]any) string {
	t.Helper()
	prg, err := relwrites.CompileStringExpr(expr)
	require.NoError(t, err, "compile %q", expr)
	v, _, err := prg.Eval(vars)
	require.NoError(t, err, "eval %q against %#v", expr, vars)
	s, ok := v.Value().(string)
	require.True(t, ok, "expected string, got %T (%v)", v.Value(), v.Value())
	return s
}

func TestGhPRRelWrites_FiresOnPrViewWithAnAuthor(t *testing.T) {
	vars := map[string]any{
		"args":    map[string]any{"argv": []any{"pr", "view", "123"}},
		"result":  map[string]any{"success": true, "stdoutJSON": map[string]any{"id": "PR_kwABC", "author": map[string]any{"id": "U_kwXYZ"}}},
		"session": "ns/sess",
	}
	assert.True(t, evalBool(t, ghPRAuthorWhenCEL, vars))
}

// A different subcommand must not fire it — the block is spec-wide, so the
// argv guard is the only thing scoping it to `pr view`.
func TestGhPRRelWrites_DoesNotFireOnAnotherSubcommand(t *testing.T) {
	vars := map[string]any{
		"args":    map[string]any{"argv": []any{"pr", "checks", "123"}},
		"result":  map[string]any{"success": true, "stdoutJSON": map[string]any{"id": "PR_kwABC", "author": map[string]any{"id": "U_kwXYZ"}}},
		"session": "ns/sess",
	}
	assert.False(t, evalBool(t, ghPRAuthorWhenCEL, vars))
}

// No stdoutJSON — a non-JSON or secretOutput call — writes nothing rather
// than erroring or writing a malformed ref.
func TestGhPRRelWrites_DoesNotFireWithoutStdoutJSON(t *testing.T) {
	vars := map[string]any{
		"args":    map[string]any{"argv": []any{"pr", "view", "123"}},
		"result":  map[string]any{"success": true},
		"session": "ns/sess",
	}
	assert.False(t, evalBool(t, ghPRAuthorWhenCEL, vars))
}

// A model that requests --json author without id is a routine, foreseeable
// under-selection (not an exotic edge case) — the when gate now excludes it
// up front instead of firing and then failing closed later at tuple.resource
// eval ("no such key: id"). Both paths are fail-closed; this one just skips
// cleanly instead of logging a per-call error for something that will happen
// often.
func TestGhPRRelWrites_DoesNotFireWithoutID(t *testing.T) {
	vars := map[string]any{
		"args":    map[string]any{"argv": []any{"pr", "view", "123"}},
		"result":  map[string]any{"success": true, "stdoutJSON": map[string]any{"author": map[string]any{"id": "U_kwXYZ"}}},
		"session": "ns/sess",
	}
	assert.False(t, evalBool(t, ghPRAuthorWhenCEL, vars))
}

// The happy path: a string node id concatenates cleanly onto the resource
// prefix.
func TestGhPRRelWrites_ResourceConcatenatesTheStringNodeID(t *testing.T) {
	vars := map[string]any{
		"result": map[string]any{"stdoutJSON": map[string]any{"id": "PR_kwABC"}},
	}
	assert.Equal(t, "github_pull_request:PR_kwABC", evalString(t, ghPRAuthorResourceCEL, vars))
}

// A NUMERIC id must fail closed — a hard eval error, no tuple — rather than
// silently concatenating into a malformed or scientific-notation ref. CEL's
// raw `+` has no _+_(string, double) overload, so a numeric id here does not
// format unusually; it errors outright. That is the failure mode this test
// exercises, and it is the reason ghPRAuthorResourceCEL must never gain a
// string() cast: a cast would turn this hard error into exactly the
// scientific-notation ref the SpiceDB object_id regex would then refuse.
func TestGhPRRelWrites_ResourceFailsClosedOnANumericID(t *testing.T) {
	prg, err := relwrites.CompileStringExpr(ghPRAuthorResourceCEL)
	require.NoError(t, err, "compile %q", ghPRAuthorResourceCEL)

	vars := map[string]any{
		"result": map[string]any{"stdoutJSON": map[string]any{"id": float64(2847362910)}},
	}
	_, _, err = prg.Eval(vars)
	require.Error(t, err, "a numeric id must fail closed rather than silently concatenate into a malformed ref")
	assert.Contains(t, err.Error(), "no such overload")
}

// The subject must name the PR's AUTHOR, never the PR itself. A subject bug
// here — binding the PR's own id, dropping the "github_user:" prefix, or
// reading .author.login instead of .author.id — would compile and run fine
// while silently writing the wrong SpiceDB relationship: nothing else in
// this file evaluates tuple.subject at all.
func TestGhPRRelWrites_SubjectIsTheAuthorID(t *testing.T) {
	vars := map[string]any{
		"result": map[string]any{"stdoutJSON": map[string]any{"id": "PR_kwABC", "author": map[string]any{"id": "U_kwXYZ"}}},
	}
	assert.Equal(t, "github_user:U_kwXYZ", evalString(t, ghPRAuthorSubjectCEL, vars))
}

// ghPRViewNoJqCEL is a hand-maintained PARALLEL COPY of a constraint carried
// by TWO YAML files — the shipped `demo-reviewbot-gh-review` toolspec
// (examples/reviewbot/manifests/toolspecs.yaml) and the e2e fixture toolspec
// (test/e2e/testdata/agent-pr-identity-e2e/02-toolspec.yaml) — same reasoning
// as ghReviewEndpointCEL (pkg/tools/toolspec/reviewbot_constraints_test.go)
// for why it is pinned here rather than read from either file. Both
// toolspecs now carry the writesRelationships blocks the ghPRAuthor*/
// ghPRRepo* trios above mirror, and this constraint is what keeps
// result.stdoutJSON, which those blocks read, un-shaped by the model. Kept
// in sync with BOTH YAML files by hand; see the matching comment on each.
//
// `gh pr view --jq EXPR` runs the JSON result through a jq program and
// REPLACES stdout with whatever that program prints — the model's own
// choice of text, not the forge's answer. ghPRAuthorResourceCEL and
// ghPRAuthorSubjectCEL (and the repo block's equivalents below) read
// result.stdoutJSON on the assumption that it IS the forge's `pr view`
// response verbatim; --jq breaks that assumption silently (a filtered
// result can still parse as JSON, so the has()-gated when clauses above
// would not necessarily catch it), letting the model shape either half of
// the tuple it is not supposed to control. This constraint denies --jq on
// `pr view` specifically — every other allowed subcommand (`pr checks`,
// `api`, `auth status`) is unaffected, because none of them feeds a
// writesRelationships block.
//
// --template is refused for the identical reason, ahead of need: `gh`'s
// --template flag runs the result through a Go template and replaces stdout
// the same way --jq does, but `pr view` does not declare it today (pinned by
// TestGhToolkit_PrViewDeclaresOnlyItsCurrentFlags,
// toolkits/gh_pr_view_flags_test.go) — so this half of the clause is INERT
// now, a live stop the day someone declares it.
const ghPRViewNoJqCEL = `call.subcommand != 'pr view' || (!call.hasFlag('jq') && !call.hasFlag('template'))`

func TestGhPRViewNoJqCEL(t *testing.T) {
	call := func(sub string, flags map[string]any) map[string]any {
		if flags == nil {
			flags = map[string]any{}
		}
		return map[string]any{"call": map[string]any{"subcommand": sub, "flags": flags}}
	}
	cases := []struct {
		name     string
		bindings map[string]any
		want     bool
	}{
		{"pr view without --jq or --template is allowed", call("pr view", nil), true},
		{"pr view with --json but no --jq or --template is allowed", call("pr view", map[string]any{"json": "id,author"}), true},
		{"pr view with --jq is refused: it lets the model reshape the forge's answer", call("pr view", map[string]any{"jq": ".author.id"}), false},
		{"pr view with --template is refused for the same reason, even though gh pr view does not declare it today", call("pr view", map[string]any{"template": "{{.author.id}}"}), false},
		{"pr checks with a jq/template-shaped flag is unaffected: the guard is scoped to pr view", call("pr checks", map[string]any{"jq": ".x", "template": "{{.x}}"}), true},
		{"api is unaffected", call("api", map[string]any{"jq": ".x"}), true},
		{"auth status is unaffected", call("auth status", nil), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, evalCEL(t, ghPRViewNoJqCEL, tc.bindings))
		})
	}
}

// ghPRRepoWhenCEL, ghPRRepoResourceCEL and ghPRRepoSubjectCEL are the same
// hand-maintained PARALLEL COPY convention as the ghPRAuthor* trio above,
// mirroring both YAML twins' SECOND writesRelationships block — the one that
// records the pull request's repository. Kept in sync with both YAML files
// by hand; see the matching comments on each block.
//
// The repo tuple's subject is the repository gh's `pr view --json
// headRepository` names, and headRepository is the FORK on a
// cross-repository (i.e. from-a-fork) pull request — verified against gh's
// own --json field list (`gh pr view --json x` prints it; there is no
// baseRepository field, in this toolkit's pinned gh revision or in gh's
// current development trunk). So this block writes the repo tuple ONLY when
// `!isCrossRepository` (head and base are the same repository) and skips it
// for a fork PR — fail-closed: the author arm above still resolves on its
// own, and no tuple names the fork as though it were the reviewed
// repository.
//
// has(result.stdoutJSON.isCrossRepository) gates the read the same way
// has(result.stdoutJSON.id) gates ghPRAuthorWhenCEL's read of .id: a model
// that requests --json without isCrossRepository (a routine partial
// selection, not an exotic one) must not make this block fire on a stale or
// absent field. The field's ABSENCE is treated as "assume cross-repository"
// — skip the write — never as "assume same-repository": an unreadable
// safety-relevant field fails closed, the same posture
// ValidateSlotBoundSubject and the slot-bound checker take everywhere else
// in this flow. See TestGhPRRepoRelWrites_DoesNotFireWithoutIsCrossRepository.
//
// has(result.stdoutJSON.headRepository) must gate
// has(result.stdoutJSON.headRepository.id) rather than the compound
// has(result.stdoutJSON.headRepository.id) alone: CEL's has() macro only
// test-guards the LAST field of the chain it is given — an absent
// headRepository makes the two-level form a hard "no such key" eval error,
// not a clean false. See TestGhPRRepoRelWrites_DoesNotFireWithoutHeadRepository.
const ghPRRepoWhenCEL = `size(args.argv) > 1 && args.argv[0] == "pr" && args.argv[1] == "view" && has(result.stdoutJSON) && has(result.stdoutJSON.id) && has(result.stdoutJSON.isCrossRepository) && !result.stdoutJSON.isCrossRepository && has(result.stdoutJSON.headRepository) && has(result.stdoutJSON.headRepository.id)`
const ghPRRepoResourceCEL = `"github_pull_request:" + result.stdoutJSON.id`
const ghPRRepoSubjectCEL = `"github_repo:" + result.stdoutJSON.headRepository.id`

func repoVars(argv []any, stdoutJSON map[string]any) map[string]any {
	return map[string]any{
		"args":    map[string]any{"argv": argv},
		"result":  map[string]any{"success": true, "stdoutJSON": stdoutJSON},
		"session": "ns/sess",
	}
}

// The happy path: a same-repository PR (head == base) fires the block.
func TestGhPRRepoRelWrites_FiresWhenNotCrossRepository(t *testing.T) {
	vars := repoVars([]any{"pr", "view", "1"}, map[string]any{
		"id": "PR_kwABC", "isCrossRepository": false,
		"headRepository": map[string]any{"id": "R_kwBASE", "name": "demo-repo"},
	})
	assert.True(t, evalBool(t, ghPRRepoWhenCEL, vars))
}

// The security property this block exists for: a fork PR must NOT write its
// fork as the pull request's repository.
func TestGhPRRepoRelWrites_DoesNotFireOnCrossRepositoryPR(t *testing.T) {
	vars := repoVars([]any{"pr", "view", "1"}, map[string]any{
		"id": "PR_kwABC", "isCrossRepository": true,
		"headRepository": map[string]any{"id": "R_kwFORK", "name": "demo-repo"},
	})
	assert.False(t, evalBool(t, ghPRRepoWhenCEL, vars))
}

// A missing isCrossRepository field must skip the write (fail closed),
// never fall through as though the PR were same-repository.
func TestGhPRRepoRelWrites_DoesNotFireWithoutIsCrossRepository(t *testing.T) {
	vars := repoVars([]any{"pr", "view", "1"}, map[string]any{
		"id":             "PR_kwABC",
		"headRepository": map[string]any{"id": "R_kwBASE", "name": "demo-repo"},
	})
	assert.False(t, evalBool(t, ghPRRepoWhenCEL, vars))
}

// A missing headRepository field (a partial --json selection) must skip
// cleanly rather than erroring at tuple.subject eval.
func TestGhPRRepoRelWrites_DoesNotFireWithoutHeadRepository(t *testing.T) {
	vars := repoVars([]any{"pr", "view", "1"}, map[string]any{
		"id": "PR_kwABC", "isCrossRepository": false,
	})
	assert.False(t, evalBool(t, ghPRRepoWhenCEL, vars))
}

// A different subcommand must not fire it — same argv-guard scoping as the
// author block, since writesRelationships is spec-wide.
func TestGhPRRepoRelWrites_DoesNotFireOnAnotherSubcommand(t *testing.T) {
	vars := repoVars([]any{"pr", "checks", "1"}, map[string]any{
		"id": "PR_kwABC", "isCrossRepository": false,
		"headRepository": map[string]any{"id": "R_kwBASE", "name": "demo-repo"},
	})
	assert.False(t, evalBool(t, ghPRRepoWhenCEL, vars))
}

// The resource is the same pull request the author block names.
func TestGhPRRepoRelWrites_ResourceConcatenatesTheStringNodeID(t *testing.T) {
	vars := map[string]any{
		"result": map[string]any{"stdoutJSON": map[string]any{"id": "PR_kwABC"}},
	}
	assert.Equal(t, "github_pull_request:PR_kwABC", evalString(t, ghPRRepoResourceCEL, vars))
}

// The subject must name the HEAD repository (== base, once the when guard
// above has confirmed same-repository), never the PR itself and never an
// author.
func TestGhPRRepoRelWrites_SubjectIsTheHeadRepositoryID(t *testing.T) {
	vars := map[string]any{
		"result": map[string]any{"stdoutJSON": map[string]any{
			"id": "PR_kwABC", "headRepository": map[string]any{"id": "R_kwBASE"},
		}},
	}
	assert.Equal(t, "github_repo:R_kwBASE", evalString(t, ghPRRepoSubjectCEL, vars))
}
