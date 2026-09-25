package sandbox

// Dispatch-level tests for the sandbox path's requireSlotBound gate. They
// call evaluateWritesRelationships directly (hence package sandbox, internal)
// the same way sandbox_tool_relwrites_stdout_test.go does, but drive the REAL
// relwrites.NewSlotBoundChecker and the REAL relwrites.Run — only the SpiceDB
// read is faked, so a conversion site that dropped the flag reddens here.

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/secretout"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/relwrites"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
)

// slotGrantListerStub stands in for (*spicedb.Client)'s ListSlotGrants.
type slotGrantListerStub struct{ grants []authz.SlotBinding }

func (s slotGrantListerStub) ListSlotGrants(_ context.Context, _, _ string) ([]authz.SlotBinding, error) {
	return s.grants, nil
}

// markedPRWriteSpec is the toolspec-form block under test: a write onto the
// pull_request named in argv, marked requireSlotBound.
func markedPRWriteSpec() spec.RelationshipWriteSpec {
	return spec.RelationshipWriteSpec{
		When:             "result.success",
		RequireSlotBound: true,
		Tuple: spec.RelationshipTupleSpec{
			Resource: `"pull_request:" + args.argv[1]`,
			Relation: `"author"`,
			Subject:  `"agentsession:" + session`,
		},
	}
}

// newSlotBoundSandboxTool builds a SandboxTool wired for a direct
// evaluateWritesRelationships call with the given grant set behind the real
// checker.
func newSlotBoundSandboxTool(t *testing.T, w relwrites.Writer, sp *spec.Spec, grants []authz.SlotBinding) *SandboxTool {
	t.Helper()
	st := newSandboxToolForRelwrites(t, w, sp)
	st.SetSlotBoundChecker(relwrites.NewSlotBoundChecker(slotGrantListerStub{grants: grants}, "default", "sess"))
	return st
}

// TestSandboxRelwrites_SlotBoundBlockWritesWhenTheGrantIsHeld proves the
// sandbox conversion site carries requireSlotBound from the toolspec into
// relwrites.Block AND that the wired checker's approval lets the tuple
// through.
func TestSandboxRelwrites_SlotBoundBlockWritesWhenTheGrantIsHeld(t *testing.T) {
	rec := &recordingRelWriter{}
	st := newSlotBoundSandboxTool(t, rec, &spec.Spec{
		WritesRelationships: []spec.RelationshipWriteSpec{markedPRWriteSpec()},
	}, []authz.SlotBinding{
		{ResourceType: "pull_request", ResourceID: authz.TrustedObjectID("owner-repo-7"), Permission: "read"},
	})

	st.evaluateWritesRelationships(context.Background(),
		tool.Result{}, "", []string{"view", "owner-repo-7"}, testSession(t))

	require.Len(t, rec.writes, 1, "the granted tuple must reach the writer")
	assert.Equal(t, "pull_request:owner-repo-7", rec.writes[0].Resource)
}

// TestSandboxRelwrites_SlotBoundBlockIsRefusedWithoutTheGrant is the same
// dispatch with the grant absent: nothing reaches the writer, AND the refusal
// reaches the agent.
//
// The second half is the point. The sandbox path swallows an ordinary
// relwrites failure by design — the call succeeded and the agent already has
// that answer — but a slot-bound refusal is an AUTHORIZATION outcome, not a
// store hiccup: the tool's answer is now one the agent holds no authority
// over, and a refusal nobody is told about looks exactly like a write nobody
// attempted. Concretely: `pr view` succeeds, the identity tuple is refused,
// view_memory stays empty, and nothing anywhere says why.
func TestSandboxRelwrites_SlotBoundBlockIsRefusedWithoutTheGrant(t *testing.T) {
	rec := &recordingRelWriter{}
	st := newSlotBoundSandboxTool(t, rec, &spec.Spec{
		WritesRelationships: []spec.RelationshipWriteSpec{markedPRWriteSpec()},
	}, []authz.SlotBinding{
		// A grant on a DIFFERENT pull request: the session holds slots, just
		// not this one.
		{ResourceType: "pull_request", ResourceID: authz.TrustedObjectID("owner-repo-8"), Permission: "read"},
	})

	got := st.evaluateWritesRelationships(context.Background(),
		tool.Result{Content: "the pr view output"}, "", []string{"view", "owner-repo-7"}, testSession(t))

	assert.Empty(t, rec.writes, "an unbound resource's tuple must never reach the writer; got %+v", rec.writes)
	assert.True(t, got.IsError, "a slot-bound refusal must reach the agent, not only the log")
	assert.Contains(t, got.Content, "pull_request:owner-repo-7", "the refusal must name the resource")
	assert.Contains(t, got.Content, "no slot-bound grant")
}

// TestSandboxRelwrites_SlotBoundRefusalClearsACapturedSecret proves the
// substituted error string can never be filed as a captured credential.
//
// The runner (loop_secretout.go) reads Content unconditionally whenever
// SecretOutput is non-nil, so replacing Content while leaving SecretOutput set
// would publish this refusal text into the per-session Secret store as though
// it were the secret the call captured. evaluateObserves clears it for exactly
// this reason; so must this.
func TestSandboxRelwrites_SlotBoundRefusalClearsACapturedSecret(t *testing.T) {
	rec := &recordingRelWriter{}
	block := markedPRWriteSpec()
	// A secretOutput toolspec sees only {success}, so the resource comes from
	// argv — which is the sre-pinning shape: capture a credential, then pin.
	st := newSlotBoundSandboxTool(t, rec, &spec.Spec{
		SecretOutput:        &spec.SecretOutputSpec{Name: "kubeconfig"},
		WritesRelationships: []spec.RelationshipWriteSpec{block},
	}, nil)

	got := st.evaluateWritesRelationships(context.Background(),
		tool.Result{Content: "SUPER-SECRET-VALUE", SecretOutput: &secretout.Spec{Name: "kubeconfig"}},
		"", []string{"view", "owner-repo-7"}, testSession(t))

	require.True(t, got.IsError)
	assert.Nil(t, got.SecretOutput, "a substituted error must not be diverted into the Secret store")
	assert.NotContains(t, got.Content, "SUPER-SECRET-VALUE")
}

// TestSandboxRelwrites_NonSlotBoundFailureStillDoesNotFlipTheResult pins the
// OTHER side of the line: a plain store failure is still logged and swallowed,
// exactly as before. Without this row, flipping on every relwrites error would
// pass the refusal test above while silently widening the change.
func TestSandboxRelwrites_NonSlotBoundFailureStillDoesNotFlipTheResult(t *testing.T) {
	block := markedPRWriteSpec()
	block.RequireSlotBound = false // unmarked: the checker is never consulted

	st := newSlotBoundSandboxTool(t, errorRelWriter{}, &spec.Spec{
		WritesRelationships: []spec.RelationshipWriteSpec{block},
	}, nil)

	got := st.evaluateWritesRelationships(context.Background(),
		tool.Result{Content: "the pr view output"}, "", []string{"view", "owner-repo-7"}, testSession(t))

	assert.False(t, got.IsError, "a store failure is not an authorization outcome; the Result is unchanged")
	assert.Equal(t, "the pr view output", got.Content)
}

// errorRelWriter fails every write, standing in for a SpiceDB hiccup.
type errorRelWriter struct{}

func (errorRelWriter) WriteRelationships(_ context.Context, _ []relwrites.ResolvedTuple) error {
	return fmt.Errorf("rpc error: code = Unavailable")
}

// TestSandboxRelwrites_UnmarkedBlockStillWritesWithoutAnyGrant pins the
// default: an unmarked block writes with no grant and no checker consulted.
// Without this row, marking every block unconditionally would still pass the
// two rows above.
func TestSandboxRelwrites_UnmarkedBlockStillWritesWithoutAnyGrant(t *testing.T) {
	block := markedPRWriteSpec()
	block.RequireSlotBound = false

	rec := &recordingRelWriter{}
	st := newSlotBoundSandboxTool(t, rec, &spec.Spec{
		WritesRelationships: []spec.RelationshipWriteSpec{block},
	}, nil)

	st.evaluateWritesRelationships(context.Background(),
		tool.Result{}, "", []string{"view", "owner-repo-7"}, testSession(t))

	require.Len(t, rec.writes, 1, "an unmarked block must write with no grant at all")
	assert.Equal(t, "pull_request:owner-repo-7", rec.writes[0].Resource)
}

// unmarkedGithubUserSubjectSpec is a block that names a github_user subject —
// a type that requires a slot-bound block (reserved_types.go's
// slotBoundRequiredSubjectTypes) — but never sets requireSlotBound: true. A
// manifest author forgetting the flag is the failure this pins, not an
// adversarial one.
func unmarkedGithubUserSubjectSpec() spec.RelationshipWriteSpec {
	return spec.RelationshipWriteSpec{
		When: "result.success",
		Tuple: spec.RelationshipTupleSpec{
			Resource: `"github_pull_request:" + args.argv[1]`,
			Relation: `"author"`,
			Subject:  `"github_user:U_kwXYZ"`,
		},
	}
}

// TestSandboxRelwrites_UnmarkedGithubUserSubjectBlockIsRefused proves the
// structural-tie refusal (ValidateSlotBoundSubject, called from inside
// Evaluate for EVERY block regardless of RequireSlotBound) surfaces to the
// agent exactly like a filterSlotBound refusal does, rather than being
// swallowed as an ordinary "evaluate failed" mechanism error.
//
// Before this test's fix, this case took the sandbox switch's default arm:
// logged at INFO, tool.Result left untouched. That is the silent-refusal
// shape the requireSlotBound feature exists to close — a manifest naming a
// github_user subject without the flag would be refused on every call with
// zero visibility, indistinguishable from a tuple nobody tried to write.
func TestSandboxRelwrites_UnmarkedGithubUserSubjectBlockIsRefused(t *testing.T) {
	rec := &recordingRelWriter{}
	st := newSandboxToolForRelwrites(t, rec, &spec.Spec{
		WritesRelationships: []spec.RelationshipWriteSpec{unmarkedGithubUserSubjectSpec()},
	})

	got := st.evaluateWritesRelationships(context.Background(),
		tool.Result{Content: "the pr view output"}, "", []string{"view", "PR_kwABC"}, testSession(t))

	assert.Empty(t, rec.writes, "a github_user subject from an unmarked block must never reach the writer")
	assert.True(t, got.IsError, "the misconfiguration must reach the agent, not only the log")
	assert.Contains(t, got.Content, "requires a slot-bound block", "the refusal must name what is wrong, not just that something failed")
	assert.Contains(t, got.Content, "github_user")
}

// TestSandboxRelwrites_UnmarkedGithubUserSubjectRefusalClearsACapturedSecret
// pins the same secret-clearing property TestSandboxRelwrites_
// SlotBoundRefusalClearsACapturedSecret proves for a filterSlotBound
// refusal: the runner (loop_secretout.go) reads Content unconditionally
// whenever SecretOutput is non-nil, so a substituted refusal string must
// never be diverted into the Secret store as though it were the captured
// credential.
func TestSandboxRelwrites_UnmarkedGithubUserSubjectRefusalClearsACapturedSecret(t *testing.T) {
	rec := &recordingRelWriter{}
	st := newSandboxToolForRelwrites(t, rec, &spec.Spec{
		SecretOutput:        &spec.SecretOutputSpec{Name: "kubeconfig"},
		WritesRelationships: []spec.RelationshipWriteSpec{unmarkedGithubUserSubjectSpec()},
	})

	got := st.evaluateWritesRelationships(context.Background(),
		tool.Result{Content: "SUPER-SECRET-VALUE", SecretOutput: &secretout.Spec{Name: "kubeconfig"}},
		"", []string{"view", "PR_kwABC"}, testSession(t))

	require.True(t, got.IsError)
	assert.Nil(t, got.SecretOutput, "a substituted error must not be diverted into the Secret store")
	assert.NotContains(t, got.Content, "SUPER-SECRET-VALUE")
}
