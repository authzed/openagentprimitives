package meta

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// Spawning a child is a consequential action the plan gate must govern:
// without it a parent under injection escapes its own plan by delegating a
// write it cannot itself perform. But delegate reaches no SpiceDB resource, so
// a Readwrite/External dispatch permission would be denied fail-closed by the
// tool checker or force per-call approval in every mode. delegate stays
// Stateless for DISPATCH and opts into plan-gate governance via the
// PlanGateGoverned marker: PlanGateHandle yields tool:delegate so the gate
// sees the spawn under enforcing mode, while dispatch does no check and no
// approval.
func TestDelegateTool_IsPlanGateGovernedButStatelessAtDispatch(t *testing.T) {
	dt := &delegateTool{}

	assert.Equal(t, authz.Stateless, dt.Permission().StateImpact,
		"dispatch stays Stateless: no per-call SpiceDB check, no per-call approval, zero blast radius")

	// The dispatch handle is empty (Stateless), but the PLAN GATE handle is not.
	_, ok := tool.BaseHandle(dt)
	assert.False(t, ok, "a Stateless tool mints no dispatch handle")

	assert.True(t, dt.PlanGateGoverned(), "delegate opts into plan-gate governance")
	h, ok := tool.PlanGateHandle(dt)
	require.True(t, ok, "the plan gate must be handed a handle so it governs the spawn")
	assert.Equal(t, "tool:delegate", h.String())

	assert.Nil(t, dt.PermissionVariants(), "delegate has no per-call variants")
}

// tl.Execute takes a third *tool.SessionContext argument per the tool.Tool
// interface (pkg/agent/tool/tool.go); every other meta-tool test in this
// package passes nil when the tool under test doesn't read it (see e.g.
// complete_phase_test.go, query_memory_test.go). delegate never reads sess --
// DelegateConfig already carries Namespace/SessionName directly -- so nil is
// passed below.

func TestDelegateTool_ReturnsTheChildResultOnSuccess(t *testing.T) {
	created := make(chan *v1.SubagentRequest, 1)
	tl := NewDelegateTool(DelegateConfig{
		Namespace: "ns", SessionName: "demo-parent",
		Create: func(_ context.Context, sr *v1.SubagentRequest) error { created <- sr; return nil },
		Poll: func(_ context.Context, _ string) (*v1.SubagentRequest, error) {
			return &v1.SubagentRequest{Status: v1.SubagentRequestStatus{
				Phase: v1.SubagentRequestPhaseSucceeded, Result: "the answer",
			}}, nil
		},
		PollInterval: time.Millisecond,
	})

	res, err := tl.Execute(context.Background(), json.RawMessage(`{"agent":"demo-coder","task":"do the thing"}`), nil)
	require.NoError(t, err)
	assert.False(t, res.IsError, "a succeeded delegation is not an error")
	assert.Contains(t, res.Content, "the answer")

	sr := <-created
	assert.Equal(t, "demo-coder", sr.Spec.Class)
	assert.Equal(t, "do the thing", sr.Spec.Task)
	assert.Equal(t, "demo-parent", sr.Spec.Parent.Name)
}

func TestDelegateTool_DenialIsTerminalNotRetryable(t *testing.T) {
	tl := NewDelegateTool(DelegateConfig{
		Namespace: "ns", SessionName: "demo-parent",
		Create: func(_ context.Context, _ *v1.SubagentRequest) error { return nil },
		Poll: func(_ context.Context, _ string) (*v1.SubagentRequest, error) {
			return &v1.SubagentRequest{Status: v1.SubagentRequestStatus{
				Phase:         v1.SubagentRequestPhaseDenied,
				FailureReason: "OffRoster",
				Determination: `"demo-sre" is not in "demo-lead"'s subagents roster`,
			}}, nil
		},
		PollInterval: time.Millisecond,
	})

	res, err := tl.Execute(context.Background(), json.RawMessage(`{"agent":"demo-sre","task":"x"}`), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError, "a denial must reach the model as an error")
	assert.Contains(t, res.Content, "not in")
	assert.Contains(t, res.Content, "Do not retry",
		"a denial must tell the model not to route around it; a retryable-looking denial is laundering")
}

// TestDelegateTool_PassesTheRequestedModeThroughUnaltered covers the field that
// makes the conversational modes reachable at all. Before it existed the tool
// built a spec with no Mode, so every delegation in the tree was single_turn no
// matter what the agent asked for.
//
// The tool substitutes nothing here: what the agent named is what the
// controller gets to authorize, so the refusal it may hand back names the mode
// the agent actually wrote.
func TestDelegateTool_PassesTheRequestedModeThroughUnaltered(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{
			name: "an explicit chat request lands on spec.mode verbatim",
			args: `{"agent":"demo-coder","task":"x","mode":"chat"}`,
			want: v1.SubagentModeChat,
		},
		{
			name: "an omitted mode leaves spec.mode empty, which the CRD reads as single_turn",
			args: `{"agent":"demo-coder","task":"x"}`,
			want: "",
		},
		{
			name: "surrounding whitespace is trimmed rather than sent as an unknown value",
			args: `{"agent":"demo-coder","task":"x","mode":"  task  "}`,
			want: v1.SubagentModeTask,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			created := make(chan *v1.SubagentRequest, 1)
			tl := NewDelegateTool(DelegateConfig{
				Namespace: "ns", SessionName: "demo-parent",
				Create: func(_ context.Context, sr *v1.SubagentRequest) error { created <- sr; return nil },
				Poll: func(_ context.Context, _ string) (*v1.SubagentRequest, error) {
					return &v1.SubagentRequest{Status: v1.SubagentRequestStatus{
						Phase: v1.SubagentRequestPhaseSucceeded, Result: "the answer",
					}}, nil
				},
				PollInterval: time.Millisecond,
			})

			res, err := tl.Execute(context.Background(), json.RawMessage(tc.args), nil)
			require.NoError(t, err)
			require.False(t, res.IsError, "a well-formed mode must not be refused by the tool: %s", res.Content)

			sr := <-created
			assert.Equal(t, tc.want, sr.Spec.Mode)
		})
	}
}

// TestDelegateTool_RefusesAnUnrecognizedModeWithoutCreatingAnything is an
// argument-shape check, NOT the authorization gate -- whether this session may
// use a mode is the parent roster's answer and the SubagentRequest
// controller's to give. What it buys is that a misspelling never reaches the
// apiserver's spec.mode Enum as a schema error the model cannot act on, and
// that it is refused rather than blanked: silently dropping it would run a
// single_turn delegation the agent never asked for.
func TestDelegateTool_RefusesAnUnrecognizedModeWithoutCreatingAnything(t *testing.T) {
	var creates int
	tl := NewDelegateTool(DelegateConfig{
		Namespace: "ns", SessionName: "demo-parent",
		Create: func(_ context.Context, _ *v1.SubagentRequest) error { creates++; return nil },
		Poll: func(_ context.Context, _ string) (*v1.SubagentRequest, error) {
			return &v1.SubagentRequest{Status: v1.SubagentRequestStatus{
				Phase: v1.SubagentRequestPhaseSucceeded, Result: "the answer",
			}}, nil
		},
		PollInterval: time.Millisecond,
	})

	res, err := tl.Execute(context.Background(), json.RawMessage(`{"agent":"demo-coder","task":"x","mode":"chatt"}`), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError, "an unrecognized mode must come back as an error")
	assert.Contains(t, res.Content, "chatt", "the model must be told which value it got wrong")
	assert.Contains(t, res.Content, v1.SubagentModeChat, "the model must be told which values exist")
	assert.Zero(t, creates, "a request the tool already knows is malformed must never be created")
}

func TestDelegateTool_RejectsAnEmptyTask(t *testing.T) {
	tl := NewDelegateTool(DelegateConfig{Namespace: "ns", SessionName: "demo-parent"})
	res, err := tl.Execute(context.Background(), json.RawMessage(`{"agent":"demo-coder","task":""}`), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content, "task")
}

func TestDelegateTool_FailureIsRetryable(t *testing.T) {
	tl := NewDelegateTool(DelegateConfig{
		Namespace: "ns", SessionName: "demo-parent",
		Create: func(_ context.Context, _ *v1.SubagentRequest) error { return nil },
		Poll: func(_ context.Context, _ string) (*v1.SubagentRequest, error) {
			return &v1.SubagentRequest{Status: v1.SubagentRequestStatus{
				Phase:         v1.SubagentRequestPhaseFailed,
				FailureReason: "ChildTimedOut",
				Determination: "the child session did not complete within its budget",
			}}, nil
		},
		PollInterval: time.Millisecond,
	})

	res, err := tl.Execute(context.Background(), json.RawMessage(`{"agent":"demo-coder","task":"x"}`), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content, "may be retried")
	assert.NotContains(t, res.Content, "Do not retry",
		"a failure must not read like a denial -- only Denied is non-retryable")
}

func TestDelegateTool_TimesOutRatherThanHangingOnAnUnrecognizedPhase(t *testing.T) {
	tl := NewDelegateTool(DelegateConfig{
		Namespace: "ns", SessionName: "demo-parent",
		Create: func(_ context.Context, _ *v1.SubagentRequest) error { return nil },
		Poll: func(_ context.Context, _ string) (*v1.SubagentRequest, error) {
			// A phase IsTerminal() does not recognize either -- proves the poll
			// loop's own deadline bounds it rather than relying on IsTerminal.
			return &v1.SubagentRequest{Status: v1.SubagentRequestStatus{Phase: "SomeFuturePhase"}}, nil
		},
		PollInterval: time.Millisecond,
		Timeout:      20 * time.Millisecond,
	})

	type outcome struct {
		res tool.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := tl.Execute(context.Background(), json.RawMessage(`{"agent":"demo-coder","task":"x"}`), nil)
		done <- outcome{res: res, err: err}
	}()

	select {
	case got := <-done:
		require.NoError(t, got.err)
		assert.True(t, got.res.IsError)
		assert.Contains(t, got.res.Content, "timed out")
	case <-time.After(2 * time.Second):
		t.Fatal("delegate hung instead of timing out on an unrecognized phase")
	}
}

// TestDelegateOwnerReference_NamesTheParentWithItsUID is the covering test
// for the orphaned-child-on-parent-deletion fix: both RunnerEnv wiring sites
// (internal/cmd/runner/main.go, the e2e in-process factory) stamp this owner
// reference onto every SubagentRequest they create, before Create, so
// Kubernetes' cascading GC reaps the request -- and transitively the child --
// when the parent AgentSession is deleted.
//
// Asserts on UID specifically, not just Name/Kind: an owner reference with a
// stale or empty UID does not cascade-GC (apiserver GC matches by UID, not
// name alone), and that failure mode is invisible until something is
// actually deleted -- exactly the risk DelegateOwnerReference exists to
// close, so the test must pin the field that would silently defeat it.
func TestDelegateOwnerReference_NamesTheParentWithItsUID(t *testing.T) {
	parent := &v1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      "demo-parent",
			UID:       types.UID("demo-parent-uid"),
		},
	}

	ref := DelegateOwnerReference(parent)

	assert.Equal(t, "AgentSession", ref.Kind)
	assert.Equal(t, "demo-parent", ref.Name)
	assert.Equal(t, types.UID("demo-parent-uid"), ref.UID,
		"the owner ref must carry the PARENT's real UID -- a stale or empty UID does not cascade-GC")
	require.NotNil(t, ref.Controller)
	assert.True(t, *ref.Controller, "must be a CONTROLLER ref for cascading GC to apply")
}
