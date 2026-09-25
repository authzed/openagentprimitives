package sandbox_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/operations"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/sandbox"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/factcontent"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/observedfact"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"

	// Registers every fact Kind (including observedfact) with the memory
	// package's Kind registry — WriteAuthority, AppendOnly and the rest are
	// derived from that registration, so a real Record/ForSubject round trip
	// needs it the same way pkg/memory/kinds/factcontent/accessor_test.go
	// does (see its newMem helper).
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
)

// gitlikeGHStdout is the JSON `gh pr view 6 --json
// number,headRefOid,headRepository,isCrossRepository` would print for a
// pull request whose head lives in the SAME repository — the motivating
// gitlike_gh shape.
const gitlikeGHStdout = `{"number":6,"headRefOid":"ba03f5969a","headRepository":{"nameWithOwner":"demo-org/demo-repo"},"isCrossRepository":false}`

// readCtx mints a context carrying a system ReadMemory approval for the
// assertions' own factcontent.ForSubject (Query) calls. Local.Query checks
// ReadMemory unconditionally (pkg/memory/facade.go:516) regardless of Kind,
// so a bare context.Background() fails before ever reaching the append-only
// question these tests are about — the same reason
// pkg/memory/kinds/factcontent/accessor_test.go's newMem helper mints one.
// Distinct from evaluateObserves' own "sandbox:observe" mint (RULING T2-D):
// that one is the ctx the PRODUCTION code path carries; this one belongs to
// the TEST's own read-back assertions and would exist even if evaluateObserves
// needed no approval at all.
func readCtx() context.Context {
	return memory.WithSystemApproval(context.Background(), "test")
}

// observesSpec is the gitlike_gh shape: `gh pr view` whose stdout JSON names
// the pull request, its head commit, and whether the head is cross-repository.
//
// `result.stdoutJSON` is the real binding evaluateObserves exposes: sandbox_tool.go's
// shared CEL vars (sandboxCELVars, ~line 620) carry only {"args":{"argv":[...]},
// "result":{"success":bool},"session":"<ns>/<name>"} — no tool content, ever,
// for evaluateWritesRelationships. evaluateObserves adds exactly one further
// key, `result.stdoutJSON`, and ONLY when the toolspec does not declare
// SecretOutput (see TestSandboxToolObserves_SecretOutputSeesOnlySuccess).
func observesSpec() *spec.Spec {
	return &spec.Spec{
		Name:             "demo-gh-review",
		Version:          "1",
		Toolkit:          spec.ToolkitRef{Name: "gh", Revision: "2026-05-01"},
		AllowSubcommands: []string{"pr view"},
		Observes: []spec.ObservesSpec{{
			ForEach: "[result.stdoutJSON]",
			Subjects: []spec.ObserveSubjectSpec{
				{ResourceType: `"git_commit"`, ResourceID: `item.headRefOid`},
				{ResourceType: `"github_pr"`, ResourceID: `item.headRepository.nameWithOwner + "#" + string(item.number)`},
			},
			Facts: map[string]string{"is_cross_repository": `item.isCrossRepository`},
		}},
	}
}

// observeFixture bundles the SandboxTool under test with the fake k8s client
// and SessionContext runToolCall needs to drive a ToolCall to a terminal
// state. sandbox.SandboxTool itself holds no reference to either — a real
// SessionContext is supplied fresh on every Execute call by the runner — so
// the test fixture must carry them alongside it.
type observeFixture struct {
	c    client.Client
	sess *tool.SessionContext
	reg  *operations.Registry
	st   *sandbox.SandboxTool
	n    int // distinguishes repeat calls' ToolCall names (T2-D test)
}

// newObserveSandboxTool builds a SandboxTool wired with sp, SetMemory'd with
// a REAL in-memory facade (memory.NewLocal(inmem.NewBackend())) — mirrors
// pkg/memory/kinds/factcontent/accessor_test.go's newMem helper: a fake
// memory.Memory would assert nothing here, since the point of these tests is
// exercising the real append-only door (RULING T2-D) and the real
// observedfact Kind registration, not a stand-in for either. SetMemory is
// the same setter internal/cmd/runner/main.go calls in production
// (alongside SetRelWriter/SetLogger) — see the RULING T5-B note in the task
// report.
func newObserveSandboxTool(t *testing.T, sp *spec.Spec) (memory.Memory, memory.Scope, *observeFixture) {
	t.Helper()
	// Seeded unconditionally, not just for the secretOutput case: ANY
	// SecretOutput-declaring spec makes ExecuteWithIDs Get this AgentSession
	// for its write-once fast-fail (sandbox_tool.go ~line 348) BEFORE the
	// ToolCall is even created — a step that has nothing to do with observe
	// and is easy to miss (a prior version of this fixture omitted it, which
	// made TestSandboxToolObserves_SecretOutputSeesOnlySuccess pass for the
	// wrong reason: a "not found" Get error, never reaching evaluateObserves
	// at all). Harmless for every other spec here, since AgentSession status
	// is otherwise unread by this path.
	c := newFakeClient(t, &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "sess"},
	})
	mem := memory.NewLocal(inmem.NewBackend())
	reg := operations.New(nil, nil)
	sess := &tool.SessionContext{
		Namespace:      "default",
		Name:           "sess",
		K8sClient:      c,
		ArtifactClient: &fakeArtifact{contents: map[string]string{"mem://stdout": gitlikeGHStdout}},
		BundleSessions: map[string]string{"code": "sess-code"},
		Operations:     reg,
	}
	stool := sandbox.NewSandboxTool(sandbox.SandboxOpts{
		BundleName:   "code",
		Suffix:       "gh",
		ToolspecName: sp.Name,
		Timeout:      30 * time.Second,
		PollInterval: 5 * time.Millisecond,
		Spec:         sp,
	})
	stool.SetMemory(mem)
	scope := memory.Scope{Kind: "session", ID: "default/sess"}
	return mem, scope, &observeFixture{c: c, sess: sess, reg: reg, st: stool}
}

// runToolCall drives one ToolCall through the real ExecuteWithIDs path — the
// production dispatch route — to a terminal state and returns the composed
// tool.Result. mutate sets ExitCode (and may set Conditions explicitly); when
// it leaves Conditions unset, the terminal condition is derived from
// ExitCode (0 => Succeeded, else => Failed), the same mapping the real
// ToolCall controller applies.
//
// Every call gets a fresh tool_use id, so two calls with identical args (the
// T2-D repeat-observation test) create two distinct ToolCall CRs — exactly
// what two real dispatches of the same tool would do — while still asserting
// the SAME fact twice, which is what exercises observedfact.Record's
// repeat-observation path.
func runToolCall(t *testing.T, f *observeFixture, mutate func(tc *spiceboxv1alpha1.ToolCall)) tool.Result {
	t.Helper()
	f.n++
	toolUseID := fmt.Sprintf("tu-obs-%d", f.n)
	op := f.reg.Begin("view pull request")

	patchToolCallStatus(t, f.c,
		client.ObjectKey{Namespace: "default", Name: fmt.Sprintf("sess-1-%s", toolUseID)},
		func(tc *spiceboxv1alpha1.ToolCall) {
			mutate(tc)
			tc.Status.StdoutArtifactRef = "mem://stdout"
			if len(tc.Status.Conditions) == 0 {
				cond := spiceboxv1alpha1.ToolCallConditionSucceeded
				if tc.Status.ExitCode == nil || *tc.Status.ExitCode != 0 {
					cond = spiceboxv1alpha1.ToolCallConditionFailed
				}
				tc.Status.Conditions = []metav1.Condition{{
					Type:               cond,
					Status:             metav1.ConditionTrue,
					Reason:             spiceboxv1alpha1.ReasonProcessExited,
					LastTransitionTime: metav1.Now(),
				}}
			}
		})

	ctx, cancel := context.WithTimeout(context.Background(), testCtxTimeout)
	defer cancel()
	body, err := json.Marshal(map[string]any{
		"operation_id": op.ID,
		"_reason":      "check whether the pull request head is a fork",
		"args":         []string{"pr", "view", "6", "demo-org/demo-repo"},
	})
	require.NoError(t, err, "marshal body")

	res, err := f.st.ExecuteWithIDs(ctx, json.RawMessage(body), f.sess, sandbox.IDs{TurnIndex: 1, ToolUseID: toolUseID})
	require.NoError(t, err, "ExecuteWithIDs must succeed")
	return res
}

// TestSandboxToolObserves covers the two post-Succeeded cases from the Task 6
// brief: a successful ToolCall records the declared facts against both
// co-derived subjects, and a failed ToolCall records nothing.
func TestSandboxToolObserves(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(tc *spiceboxv1alpha1.ToolCall)
		wantFacts map[string]any // nil => expect nothing recorded
		// wantContentContains, when set, is positive evidence of WHICH path
		// produced the empty-facts result: every early return in
		// ExecuteWithIDs (a bad body, a missing SpiceboxSession, ...) also
		// yields no recorded facts, so an empty-facts assertion alone cannot
		// tell "the errored-call guard in evaluateObserves/composeResult ran"
		// apart from "the call never got that far". A substring unique to
		// composeToolCallResult's own failure branch pins the former.
		wantContentContains string
	}{
		{
			name: "ToolCall reaches Succeeded: facts recorded for both subjects",
			mutate: func(tc *spiceboxv1alpha1.ToolCall) {
				exit := int32(0)
				tc.Status.ExitCode = &exit
			},
			wantFacts: map[string]any{"is_cross_repository": false},
		},
		{
			name: "ToolCall failed: nothing recorded, because a failed call asserts nothing",
			mutate: func(tc *spiceboxv1alpha1.ToolCall) {
				exit := int32(1)
				tc.Status.ExitCode = &exit
			},
			wantFacts:           nil,
			wantContentContains: "ToolCall failed:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mem, scope, f := newObserveSandboxTool(t, observesSpec())
			res := runToolCall(t, f, tc.mutate)
			if tc.wantFacts != nil {
				require.False(t, res.IsError, "unexpected IsError; content=%s", res.Content)
			} else {
				assert.True(t, res.IsError, "expected the failed ToolCall to compose an IsError result")
			}
			if tc.wantContentContains != "" {
				assert.Contains(t, res.Content, tc.wantContentContains,
					"must show composeResult actually reached its failure branch, not an earlier unrelated return")
			}

			for _, subj := range []factcontent.Subject{
				{ResourceType: "git_commit", ResourceID: "ba03f5969a"},
				{ResourceType: "github_pr", ResourceID: "demo-org/demo-repo#6"},
			} {
				// observedfact.ForSubject, not the generic factcontent.ForSubject
				// with a hand-passed KindName — RULING T1-C: this Kind's own
				// wrapper is what every call site outside factcontent/observedfact
				// themselves should use.
				got, err := observedfact.ForSubject(readCtx(), mem, scope, subj.ResourceType, subj.ResourceID)
				require.NoError(t, err)
				if tc.wantFacts == nil {
					assert.Empty(t, got, "subject %s:%s", subj.ResourceType, subj.ResourceID)
					continue
				}
				assert.Equal(t, tc.wantFacts, got, "subject %s:%s", subj.ResourceType, subj.ResourceID)
			}
		})
	}
}

// TestSandboxToolObserves_SecretOutputSeesOnlySuccess pins the constraint
// that must never break: a toolspec declaring secretOutput exposes only
// {"success": bool} to CEL for the observes post-effect — evaluateObserves
// never adds `result.stdoutJSON` when SecretOutput is set (sandbox_tool.go),
// so the captured secret value can never reach an observes block. A block
// that reaches for stdoutJSON anyway FAILS CEL evaluation (no such key) and
// records nothing — it does not silently record a zero value.
func TestSandboxToolObserves_SecretOutputSeesOnlySuccess(t *testing.T) {
	s := observesSpec()
	s.SecretOutput = &spec.SecretOutputSpec{Name: "token", Source: "stdout"}

	mem, scope, f := newObserveSandboxTool(t, s)
	res := runToolCall(t, f, func(tc *spiceboxv1alpha1.ToolCall) {
		exit := int32(0)
		tc.Status.ExitCode = &exit
	})
	// The call itself succeeded (secretOutput's stdout-source branch never
	// sets IsError on success) — what must be verified is that evaluateObserves
	// still refuses to record from it, because the block it declared cannot be
	// satisfied without the content that is being withheld.
	assert.True(t, res.IsError, "a block reaching for withheld content must fail closed, not silently record nothing")
	// Pins that the assertions above are actually exercising evaluateObserves'
	// own IsError flip (sandbox_tool.go:729/:749) — not some EARLIER return in
	// ExecuteWithIDs that happens to also produce IsError=true with
	// SecretOutput nil and no facts (e.g. the write-once fast-fail Get on a
	// missing AgentSession did exactly that before newObserveSandboxTool
	// seeded one; see the fix-round-1 note in the task report). This substring
	// is unique to those two flips in this file — grep confirms nothing else
	// in sandbox_tool.go produces it — so its presence is positive evidence
	// this test reached the code under test, not just consistent with having
	// reached it.
	assert.Contains(t, res.Content, "could not record what this result asserts",
		"must fail via evaluateObserves' own flip, not an earlier unrelated return")
	// Pins the fix-round-1 regression: composeToolCallResult's stdout-source
	// secretOutput branch sets res.SecretOutput, and the runner
	// (loop_secretout.go) reads res.Content unconditionally whenever
	// SecretOutput is non-nil. evaluateObserves' IsError flip MUST also clear
	// SecretOutput, or this substituted error string — not the real captured
	// stdout — gets published into the per-session Secret as the "credential".
	assert.Nil(t, res.SecretOutput, "an observe failure must not leave SecretOutput set — its Content is now an error message, not the captured secret")

	got, err := observedfact.ForSubject(readCtx(), mem, scope, "github_pr", "demo-org/demo-repo#6")
	require.NoError(t, err)
	assert.Empty(t, got, "a secretOutput toolspec must not leak its capture into a fact")
}

// TestSandboxToolObserves_RepeatCallIsNotAnError pins RULING T2-D: the ctx
// evaluateObserves passes to observedfact.Record must carry a ReadMemory
// approval, because Record's repeat-observation path Queries the store and
// Local.Query checks that door unconditionally (pkg/memory/facade.go:516).
// Without it, a second identical observation would surface as an error
// wrapping ErrAppendOnlyConflict instead of the no-op the design promises —
// an agent calling the same read-only tool twice must not see a
// contradiction it never made.
func TestSandboxToolObserves_RepeatCallIsNotAnError(t *testing.T) {
	mem, scope, f := newObserveSandboxTool(t, observesSpec())
	succeed := func(tc *spiceboxv1alpha1.ToolCall) {
		exit := int32(0)
		tc.Status.ExitCode = &exit
	}

	first := runToolCall(t, f, succeed)
	require.False(t, first.IsError, "first call: unexpected IsError; content=%s", first.Content)

	second := runToolCall(t, f, succeed)
	assert.False(t, second.IsError, "second identical observation must be a no-op, not an error; content=%s", second.Content)

	// A "not an error" second call alone would also pass if the no-op
	// silently discarded the observation instead of tolerating it — this
	// proves the FIRST observation's value is still readable afterwards, not
	// just that the second call declined to complain.
	got, err := observedfact.ForSubject(readCtx(), mem, scope, "github_pr", "demo-org/demo-repo#6")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"is_cross_repository": false}, got)
}

// TestSandboxToolObserves_RepeatCallWithANonBoolFactIsNotAnError is the same
// repeat-observation claim on a fact that is NOT a boolean, driven through the
// real CEL evaluator rather than through a hand-built value.
//
// It is a separate test because the bool cases could not have caught the bug:
// `int(item.number)` yields an int64 out of CEL, JSON hands it back as a
// float64, and a Go-level comparison of the two answers "different". The agent
// then sees an append-only conflict — the platform accusing it of contradicting
// a fact it just re-derived identically — on the second of two identical
// read-only calls. Every fact in this package's other tests is a bool, which is
// exactly why the shape needs its own pin here and not only in
// pkg/memory/kinds/factcontent.
func TestSandboxToolObserves_RepeatCallWithANonBoolFactIsNotAnError(t *testing.T) {
	sp := observesSpec()
	sp.Observes[0].Facts["pr_number"] = "int(item.number)"
	mem, scope, f := newObserveSandboxTool(t, sp)
	succeed := func(tc *spiceboxv1alpha1.ToolCall) {
		exit := int32(0)
		tc.Status.ExitCode = &exit
	}

	first := runToolCall(t, f, succeed)
	require.False(t, first.IsError, "first call: unexpected IsError; content=%s", first.Content)

	second := runToolCall(t, f, succeed)
	assert.False(t, second.IsError,
		"re-observing an int-valued fact must be a no-op, not a conflict; content=%s", second.Content)

	// Read back through JSON, so the int arrives as float64 — which is the
	// whole reason the stored and freshly-derived sides cannot be compared as
	// Go values.
	got, err := observedfact.ForSubject(readCtx(), mem, scope, "github_pr", "demo-org/demo-repo#6")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"is_cross_repository": false, "pr_number": float64(6)}, got)
}
