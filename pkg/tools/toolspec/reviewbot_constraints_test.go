package toolspec

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"

	apiv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/kubeyaml"
	toolspeccel "github.com/authzed/openagentprimitives/pkg/tools/cel"
)

// ghReviewEndpointCEL is a hand-maintained PARALLEL COPY of the `gh api`
// endpoint constraint on the gh-review SpiceboxToolspec shipped at
// examples/reviewbot/manifests/toolspecs.yaml. It exists here, as a Go
// constant, only because no test in this repo may reference anything under
// examples/ (AGENTS.md is explicit about that) — so this pins the CEL
// expression's SEMANTICS rather than reading the shipped file.
//
// The two copies are kept in sync BY HAND. Nothing derives the YAML from
// this constant or the reverse, so an edit to one that is not mirrored to
// the other will not be caught by this test, or by anything else in the
// suite — see the matching comment on the gh-review constraint in the YAML.
// TestGhReviewConstraint_AllowsCheckRunsAndRefusesEverythingElse verifies
// this expression's own logic; it is not, and cannot be, evidence that the
// shipped bundle still matches it.
//
// This CEL is also not the thing standing between reviewbot and a written
// PR comment. It is the INNER of two independent rings; the OUTER, ENFORCED
// ring is the GitHub App's permission set — contents:read, pull_requests:read,
// metadata:read, checks:write — built in
// pkg/channels/channelkinds/github/appprovision/manifest.go and pinned there
// by an exact-equality test. A token without issues:write cannot POST a PR
// comment no matter what this expression allows; this constraint is defense
// in depth behind that permission set, not a second guarantee of the same
// strength.
//
// CEL's matches() is an unanchored substring search: only the trailing `$`
// after the alternation anchors every branch's END (the leading `^` anchors
// the start). Without it, "repos/o/r/check-runs/../../../repos/o/r/issues/1/
// comments" satisfies "^repos/[^/]+/[^/]+/check-runs" as a mere PREFIX and
// would pass — see TestGhReviewConstraint_AllowsCheckRunsAndRefusesEverythingElse's
// path-traversal cases. The explicit `..` rejection is defense in depth on
// top of the anchor, not a substitute for it.
//
// It is now READ-ONLY: no method/body/hostname flags, and no POST-only
// endpoint. gh defaults to GET; `-X`/`--method` chooses any verb; `-f`/`-F`
// (`--raw-field`/`--field`) add a body and silently flip the method to POST;
// `--input` supplies a body file; `--hostname` reroutes the request. The gh
// toolkit normalizes short flags onto long names (toolkits/gh.yaml declares
// `{long: method, short: X}` etc.), so `hasFlag('method')` covers `-X` too.
// The bare `repos/*/*/check-runs` endpoint (POST-create only on GitHub's
// API) is dropped; `check-runs/[0-9]+` (single-run read) is added.
// Check-run WRITES go exclusively through the framework's trigger-status
// seam, which the App's checks:write exists for.
const ghReviewEndpointCEL = `call.subcommand != 'api' || (!call.hasFlag('method') && !call.hasFlag('field') && !call.hasFlag('raw-field') && !call.hasFlag('input') && !call.hasFlag('hostname') && !call.positional['endpoint'].contains('..') && call.positional['endpoint'].matches('^repos/[^/]+/[^/]+/(check-runs/[0-9]+|pulls/[0-9]+|pulls/[0-9]+/files|pulls/[0-9]+/commits|commits/[0-9a-f]+/check-runs)$'))`

// evalCEL compiles expr against the real toolspec CEL environment
// (pkg/tools/cel.Env — the same one the SpiceboxToolspec validator compiles
// constraints against) and evaluates it against bindings, which must be
// shaped {"call": {...}} and may carry {"config": {...}} for constraints that
// read the installer-bound config root. Every case in this file supplies a
// well-formed call, so a compile or eval error fails the test rather than
// being returned to the caller.
func evalCEL(t *testing.T, expr string, bindings map[string]any) bool {
	t.Helper()
	prog, err := toolspeccel.Compile(expr)
	require.NoError(t, err, "compile %q", expr)
	call, _ := bindings["call"].(map[string]any)
	config, _ := bindings["config"].(map[string]any)
	got, err := toolspeccel.EvalBool(prog, call, config)
	require.NoError(t, err, "eval %q against %#v", expr, bindings)
	return got
}

// loadToolspec reads the SpiceboxToolspec named name out of this package's
// own testdata/toolspecs.yaml fixture — a copy this test owns, independent
// of the shipped bundle at examples/reviewbot/manifests/toolspecs.yaml (see
// that fixture's header comment for why they are two separate files).
func loadToolspec(t *testing.T, name string) *apiv1alpha1.SpiceboxToolspec {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "toolspecs.yaml"))
	require.NoError(t, err, "read testdata/toolspecs.yaml")
	docs, err := kubeyaml.Split(data)
	require.NoError(t, err, "split testdata/toolspecs.yaml")
	for _, doc := range docs {
		if doc.GetKind() != "SpiceboxToolspec" || doc.GetName() != name {
			continue
		}
		var out apiv1alpha1.SpiceboxToolspec
		require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(doc.Object, &out),
			"convert %s to SpiceboxToolspec", name)
		return &out
	}
	t.Fatalf("toolspec %q not found in testdata/toolspecs.yaml", name)
	return nil
}

func TestGhReviewConstraint_AllowsCheckRunsAndRefusesEverythingElse(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		flags    map[string]any
		want     bool
	}{
		{name: "read check runs for a commit: allowed",
			endpoint: "repos/demo-org/platform/commits/abc123/check-runs", want: true},
		{name: "read a single check run: allowed",
			endpoint: "repos/demo-org/platform/check-runs/99044729080", want: true},
		{name: "read the PR: allowed", endpoint: "repos/demo-org/platform/pulls/42", want: true},
		{name: "read the PR files: allowed", endpoint: "repos/demo-org/platform/pulls/42/files", want: true},
		{name: "read the PR commits (author login → commit email, for the Slack mention): allowed",
			endpoint: "repos/demo-org/platform/pulls/42/commits", want: true},
		{name: "an Accept header on an allowed read: allowed",
			endpoint: "repos/demo-org/platform/pulls/42",
			flags:    map[string]any{"header": "Accept: application/vnd.github+json"}, want: true},
		{name: "a jq filter on an allowed read: allowed",
			endpoint: "repos/demo-org/platform/pulls/42",
			flags:    map[string]any{"jq": ".title"}, want: true},
		{name: "create a check run: refused — writes go through the trigger-status seam only",
			endpoint: "repos/demo-org/platform/check-runs", want: false},
		{name: "an explicit method on an allowed endpoint: refused",
			endpoint: "repos/demo-org/platform/check-runs/99044729080",
			flags:    map[string]any{"method": "PATCH"}, want: false},
		{name: "a raw-field body flips gh to POST: refused",
			endpoint: "repos/demo-org/platform/commits/abc123/check-runs",
			flags:    map[string]any{"raw-field": "output[summary]=a finding described in public"}, want: false},
		{name: "a typed field body flips gh to POST: refused",
			endpoint: "repos/demo-org/platform/pulls/42",
			flags:    map[string]any{"field": "state=closed"}, want: false},
		{name: "a body read from a file: refused",
			endpoint: "repos/demo-org/platform/pulls/42",
			flags:    map[string]any{"input": "/tmp/body.json"}, want: false},
		{name: "rerouting the request to another host: refused",
			endpoint: "repos/demo-org/platform/pulls/42",
			flags:    map[string]any{"hostname": "ghe.attacker.example"}, want: false},
		{name: "post a PR comment: refused",
			endpoint: "repos/demo-org/platform/issues/42/comments", want: false},
		{name: "submit a PR review: refused",
			endpoint: "repos/demo-org/platform/pulls/42/reviews", want: false},
		{name: "delete a branch: refused",
			endpoint: "repos/demo-org/platform/git/refs/heads/main", want: false},
		{name: "read another org: refused by the App install, but also shaped here",
			endpoint: "orgs/demo-org/members", want: false},
		{name: "path traversal disguised as a check-runs prefix: refused",
			endpoint: "repos/demo-org/platform/check-runs/../../../repos/demo-org/platform/issues/1/comments", want: false},
		{name: "unanchored suffix appended after an allowed endpoint, no dots at all: refused",
			// Proves the fix is the trailing $ anchor, not merely the `..`
			// rejection: this endpoint has no ".." anywhere, yet without the
			// anchor it would still satisfy the check-runs branch as a prefix.
			endpoint: "repos/demo-org/platform/check-runs/99/extra", want: false},
		{name: "path traversal appended after an allowed pulls/files endpoint: refused",
			endpoint: "repos/demo-org/platform/pulls/42/files/../../../issues/1/comments", want: false},
		{name: "path traversal appended after an allowed pulls/commits endpoint: refused",
			endpoint: "repos/demo-org/platform/pulls/42/commits/../../../issues/1/comments", want: false},
		{name: "unanchored suffix after pulls/commits, no dots at all: refused",
			endpoint: "repos/demo-org/platform/pulls/42/commits/extra", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flags := tc.flags
			if flags == nil {
				flags = map[string]any{}
			}
			got := evalCEL(t, ghReviewEndpointCEL, map[string]any{
				"call": map[string]any{
					"subcommand": "api",
					"positional": map[string]any{"endpoint": tc.endpoint},
					"flags":      flags,
				},
			})
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestGitRo_ForbidsEveryMutatingSubcommand(t *testing.T) {
	spec := loadToolspec(t, "git-ro")
	for _, forbidden := range []string{"push", "commit", "add", "reset", "tag", "remote"} {
		assert.NotContains(t, spec.Spec.AllowSubcommands, forbidden,
			"reviewbot must be unable to modify a repository")
	}
	for _, required := range []string{"clone", "fetch", "checkout", "diff", "log"} {
		assert.Contains(t, spec.Spec.AllowSubcommands, required)
	}
}

// ghAllowlistCEL is a hand-maintained PARALLEL COPY of the repository-allowlist
// constraint carried by BOTH the git-ro and gh-review SpiceboxToolspecs shipped
// at examples/reviewbot/manifests/toolspecs.yaml. Like ghReviewEndpointCEL
// above, it lives here as a Go constant only because no test may read examples/
// (AGENTS.md); the two copies are kept in sync by hand. This pins the CEL's
// SEMANTICS — it is not evidence the shipped bundle still matches it.
//
// call.resourceId is the toolkit-canonical, unescaped github URL the toolspec
// validator binds for a repo-bearing call (https://github.com/OWNER/REPO); it is
// "" for a local/workspace op (git status, gh auth status). config.allowedRepos
// is the operator's install answer (spec.config.allowedRepos on the AgentClass),
// exposed to constraint CEL as the `config` root. The github.com/ prefix is
// literal on purpose: it pins matches to the App's host, so a non-github (or
// off-allowlist) remote matches nothing and is refused.
const ghAllowlistCEL = `call.resourceId == '' || config.allowedRepos.exists(r, glob.match('https://github.com/' + r, call.resourceId))`

func TestReviewbotRepoAllowlistCEL(t *testing.T) {
	cases := []struct {
		name       string
		resourceID string
		allowed    []any
		want       bool
	}{
		{"exact match allowed", "https://github.com/demo-org/demo-repo", []any{"demo-org/demo-repo"}, true},
		{"org glob allowed", "https://github.com/demo-org/anything", []any{"demo-org/*"}, true},
		{"different owner refused", "https://github.com/evil/x", []any{"demo-org/*"}, false},
		{"repo outside the listed exact set refused", "https://github.com/demo-org/other", []any{"demo-org/demo-repo"}, false},
		{"empty allowlist refuses everything (fail-closed)", "https://github.com/demo-org/x", []any{}, false},
		{"local/sentinel op is not gated", "", []any{"demo-org/*"}, true},
		{"non-github host refused even under a matching org glob", "https://gitlab.com/demo-org/x", []any{"demo-org/*"}, false},
		{"multiple entries, one matches", "https://github.com/other/repo", []any{"demo-org/*", "other/repo"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evalCEL(t, ghAllowlistCEL, map[string]any{
				"call":   map[string]any{"resourceId": tc.resourceID},
				"config": map[string]any{"allowedRepos": tc.allowed},
			})
			assert.Equal(t, tc.want, got)
		})
	}
}

// ghReviewRequireRepoCEL is a hand-maintained PARALLEL COPY of the gh-review
// toolspec's require-repo constraint (examples/reviewbot/manifests/toolspecs.yaml).
// pr view / pr checks name their repository only through --repo (the {repo}
// template the check reads); a positional selector resolves call.resourceId to
// "" and would slip past the allowlist as a local op. This forces --repo so the
// repository is always named and always gated. Kept in sync by hand.
const ghReviewRequireRepoCEL = `(call.subcommand != 'pr view' && call.subcommand != 'pr checks') || call.hasFlag('repo')`

func TestReviewbotGhRequireRepoCEL(t *testing.T) {
	call := func(sub string, flags ...string) map[string]any {
		f := map[string]any{}
		for _, k := range flags {
			f[k] = "x"
		}
		return map[string]any{"call": map[string]any{"subcommand": sub, "flags": f}}
	}
	cases := []struct {
		name     string
		bindings map[string]any
		want     bool
	}{
		{"pr view without --repo is refused", call("pr view"), false},
		{"pr view with --repo is allowed", call("pr view", "repo"), true},
		{"pr checks without --repo is refused", call("pr checks"), false},
		{"pr checks with --repo is allowed", call("pr checks", "repo"), true},
		{"api is unaffected (names its repo in the endpoint)", call("api"), true},
		{"auth status is unaffected", call("auth status"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, evalCEL(t, ghReviewRequireRepoCEL, tc.bindings))
		})
	}
}
