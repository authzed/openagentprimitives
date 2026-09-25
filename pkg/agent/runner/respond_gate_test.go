package runner

import (
	"context"
	"errors"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/leakage"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// preResponseHook denies PreResponse with the configured reason.
type preResponseHook struct{ reason string }

func (h *preResponseHook) Name() string             { return "fake_preresponse" }
func (h *preResponseHook) Points() []pipeline.Point { return []pipeline.Point{pipeline.PreResponse} }
func (h *preResponseHook) Eval(_ context.Context, in pipeline.Input) pipeline.Decision {
	if in.Point != pipeline.PreResponse {
		return pipeline.Decision{}
	}
	if h.reason == "" {
		return pipeline.Decision{} // Allow
	}
	return pipeline.Decision{Verdict: pipeline.Deny, Reason: h.reason}
}

func newRespondGateLoop(t *testing.T, reg *pipeline.Registry) *Loop {
	t.Helper()
	l := &Loop{SessionKey: memory.NamespacedName{Namespace: "default", Name: "resp"}}
	l.pipelineExec = pipeline.NewExecutor(reg)
	l.pipelineOnce.Do(func() {})
	return l
}

func gateSess() *tool.SessionContext {
	return &tool.SessionContext{Namespace: "default", Name: "resp"}
}

func TestRespondGate_Allow_ReturnsNil(t *testing.T) {
	reg := pipeline.NewRegistry()
	reg.Register(&preResponseHook{reason: ""}, 10) // Allow
	l := newRespondGateLoop(t, reg)
	err := l.leakageGateForRespond(memory.WithSystemApproval(context.Background(), "test"), gateSess(), "anything", nil)
	assert.NoError(t, err)
}

func TestRespondGate_DeniedShare_ReturnsErrShareDenied(t *testing.T) {
	// A PreResponse Deny whose reason carries the [ErrShareDenied] marker must
	// surface as an error that errors.Is(.., leakage.ErrShareDenied) so
	// respond.go fires the Terminal/IdleExit yield-to-user behavior.
	reg := pipeline.NewRegistry()
	reg.Register(&preResponseHook{
		reason: "info-leakage: already denied [ErrShareDenied]: " + leakage.ErrShareDenied.Error(),
	}, 10)
	l := newRespondGateLoop(t, reg)
	err := l.leakageGateForRespond(memory.WithSystemApproval(context.Background(), "test"), gateSess(), "here is the secret", nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, leakage.ErrShareDenied),
		"denied-share marker must map back to leakage.ErrShareDenied")
}

func TestRespondGate_ExecutorError_FailsClosed(t *testing.T) {
	// A non-nil error from the PreResponse executor (a host-primitive failure
	// the executor can't turn into a verdict; Verdict defaults to Allow) must
	// NOT map to nil/publish. Fail closed: return an error so respond.go blocks
	// the outbound publish.
	l := &Loop{SessionKey: memory.NamespacedName{Namespace: "default", Name: "resp"}}
	injectRunner(t, l, &erroringRunner{failAt: pipeline.PreResponse, err: errors.New("audience store unreachable")})

	err := l.leakageGateForRespond(memory.WithSystemApproval(context.Background(), "test"), gateSess(), "secret response", nil)
	require.Error(t, err, "a PreResponse executor error must block the publish, not allow it")
	assert.False(t, errors.Is(err, leakage.ErrShareDenied),
		"an infra error is not a share denial")
	assert.Contains(t, err.Error(), "audience store unreachable", "the cause must be surfaced")
}

func TestRespondGate_GenericDeny_ReturnsPlainError(t *testing.T) {
	// A Deny without the marker is a generic (retryable) block — it must NOT
	// wrap ErrShareDenied (otherwise respond.go would wrongly yield terminally).
	reg := pipeline.NewRegistry()
	reg.Register(&preResponseHook{reason: "info-leakage: audience resolution failed"}, 10)
	l := newRespondGateLoop(t, reg)
	err := l.leakageGateForRespond(memory.WithSystemApproval(context.Background(), "test"), gateSess(), "text", nil)
	require.Error(t, err)
	assert.False(t, errors.Is(err, leakage.ErrShareDenied),
		"generic deny must not be classified as a share denial")
	assert.Contains(t, err.Error(), "audience resolution failed")
}
