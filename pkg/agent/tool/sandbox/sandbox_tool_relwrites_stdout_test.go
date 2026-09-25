package sandbox

// Tests evaluateWritesRelationships' stdout enrichment directly, calling the
// unexported method rather than driving a full ExecuteWithIDs round-trip —
// hence package sandbox (internal), not sandbox_test. The heavier-weight
// ExecuteWithIDs-driven relwrites tests (argv-only fixture, writer-error,
// write-once) live in sandbox_tool_relwrites_test.go; these are a distinct,
// lighter-weight set for the one new binding (result.stdoutJSON), per the
// Task 1 brief.

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz/relwrites"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
)

// recordingRelWriter captures every ResolvedTuple relwrites.Run hands it.
// Unlike sandbox_tool_relwrites_test.go's fakeRelWriter, it carries no
// canned-error knob — these tests only care what reached the writer.
type recordingRelWriter struct {
	mu     sync.Mutex
	writes []relwrites.ResolvedTuple
}

func (r *recordingRelWriter) WriteRelationships(_ context.Context, ts []relwrites.ResolvedTuple) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writes = append(r.writes, ts...)
	return nil
}

// newSandboxToolForRelwrites builds a SandboxTool wired for a direct
// evaluateWritesRelationships call: the given spec and rel writer only — no
// toolkit, no k8s client — since this path never drives ExecuteWithIDs.
func newSandboxToolForRelwrites(t *testing.T, w relwrites.Writer, sp *spec.Spec) *SandboxTool {
	t.Helper()
	st := NewSandboxTool(SandboxOpts{
		BundleName: "code",
		Suffix:     "test",
		Spec:       sp,
	})
	st.SetRelWriter(w)
	return st
}

// testSession returns a minimal SessionContext for CEL's `session` binding.
func testSession(t *testing.T) *tool.SessionContext {
	t.Helper()
	return &tool.SessionContext{Namespace: "default", Name: "sess"}
}

// A relwrite block can read the call's stdout, parsed, the same way an
// observes block already can.
func TestSandboxRelwrites_CanReadStdoutJSON(t *testing.T) {
	rec := &recordingRelWriter{}
	st := newSandboxToolForRelwrites(t, rec, &spec.Spec{
		WritesRelationships: []spec.RelationshipWriteSpec{{
			When: `has(result.stdoutJSON) && has(result.stdoutJSON.id)`,
			Tuple: spec.RelationshipTupleSpec{
				Resource: `"widget:" + result.stdoutJSON.id`,
				Relation: `"observed_by"`,
				Subject:  `"agentsession:" + session`,
			},
		}},
	})

	st.evaluateWritesRelationships(context.Background(),
		tool.Result{}, `{"id":"w-1"}`, []string{"show"}, testSession(t))

	require.Len(t, rec.writes, 1)
	assert.Equal(t, "widget:w-1", rec.writes[0].Resource)
}

// The secretOutput guard: a toolspec capturing a secret sees ONLY
// {success}, so no block can reach the captured bytes.
func TestSandboxRelwrites_SecretOutputSeesOnlySuccess(t *testing.T) {
	rec := &recordingRelWriter{}
	st := newSandboxToolForRelwrites(t, rec, &spec.Spec{
		SecretOutput: &spec.SecretOutputSpec{},
		WritesRelationships: []spec.RelationshipWriteSpec{{
			When: `has(result.stdoutJSON)`,
			Tuple: spec.RelationshipTupleSpec{
				Resource: `"widget:leaked"`,
				Relation: `"observed_by"`,
				Subject:  `"agentsession:" + session`,
			},
		}},
	})

	// Even handed stdout directly, the guard must refuse to expose it.
	st.evaluateWritesRelationships(context.Background(),
		tool.Result{}, `{"id":"secret"}`, []string{"show"}, testSession(t))

	assert.Empty(t, rec.writes, "a secretOutput toolspec must never expose stdout to a relationship write")
}

// Non-JSON stdout leaves stdoutJSON unset rather than setting something
// wrong, and says so in the log.
func TestSandboxRelwrites_NonJSONStdoutLeavesStdoutJSONUnset(t *testing.T) {
	rec := &recordingRelWriter{}
	st := newSandboxToolForRelwrites(t, rec, &spec.Spec{
		WritesRelationships: []spec.RelationshipWriteSpec{{
			When: `has(result.stdoutJSON)`,
			Tuple: spec.RelationshipTupleSpec{
				Resource: `"widget:x"`,
				Relation: `"observed_by"`,
				Subject:  `"agentsession:" + session`,
			},
		}},
	})

	st.evaluateWritesRelationships(context.Background(),
		tool.Result{}, "not json at all", []string{"show"}, testSession(t))

	assert.Empty(t, rec.writes)
}

// The pre-existing shape still works: a block reading only argv and session
// is unaffected by the enrichment.
func TestSandboxRelwrites_ArgvOnlyBlockIsUnchanged(t *testing.T) {
	rec := &recordingRelWriter{}
	st := newSandboxToolForRelwrites(t, rec, &spec.Spec{
		WritesRelationships: []spec.RelationshipWriteSpec{{
			Tuple: spec.RelationshipTupleSpec{
				Resource: `"cluster:" + args.argv[1]`,
				Relation: `"debug_target"`,
				Subject:  `"agentsession:" + session`,
			},
		}},
	})

	st.evaluateWritesRelationships(context.Background(),
		tool.Result{}, "", []string{"debug", "c-1"}, testSession(t))

	require.Len(t, rec.writes, 1)
	assert.Equal(t, "cluster:c-1", rec.writes[0].Resource)
}
