package hooks

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

func forkInput(forker string) pipeline.Input {
	return pipeline.Input{
		Point:     pipeline.SessionFork,
		Session:   pipeline.SessionRef{Namespace: "ns", Name: "parent"},
		Requester: identity.CanonicalFromTrusted(forker, "test fixture"),
		Fork: &pipeline.ForkInfo{
			ParentRef: "ns/parent", ChildRef: "ns/child", Forker: forker, CutTurn: 2,
		},
	}
}

func TestSessionFork_Eval(t *testing.T) {
	t.Run("owner: Allow, no notice", func(t *testing.T) {
		h := NewSessionFork(SessionForkDeps{
			CheckFork: func(_ context.Context, _, _ string, _ identity.CanonicalUserID) (bool, error) { return true, nil },
		})
		dec := h.Eval(context.Background(), forkInput("user:alice"))
		assert.Equal(t, pipeline.Allow, dec.Verdict)
		assert.Empty(t, dec.Notices)
	})

	t.Run("non-owner: Deny with requester notice + reason", func(t *testing.T) {
		h := NewSessionFork(SessionForkDeps{
			CheckFork: func(_ context.Context, _, _ string, _ identity.CanonicalUserID) (bool, error) { return false, nil },
		})
		dec := h.Eval(context.Background(), forkInput("user:bob"))
		assert.Equal(t, pipeline.Deny, dec.Verdict)
		assert.NotEmpty(t, dec.Reason)
		require.Len(t, dec.Notices, 1)
		assert.True(t, dec.Notices[0].ToRequester)
		assert.NotEmpty(t, dec.Notices[0].Text)
	})

	t.Run("checker error: Deny (fail-closed) with notice", func(t *testing.T) {
		h := NewSessionFork(SessionForkDeps{
			CheckFork: func(_ context.Context, _, _ string, _ identity.CanonicalUserID) (bool, error) {
				return false, errors.New("spicedb down")
			},
		})
		dec := h.Eval(context.Background(), forkInput("user:alice"))
		assert.Equal(t, pipeline.Deny, dec.Verdict)
		require.Len(t, dec.Notices, 1)
	})

	t.Run("nil checker: Deny (fail-closed)", func(t *testing.T) {
		h := NewSessionFork(SessionForkDeps{CheckFork: nil})
		dec := h.Eval(context.Background(), forkInput("user:alice"))
		assert.Equal(t, pipeline.Deny, dec.Verdict)
	})

	t.Run("metadata: Name + Points", func(t *testing.T) {
		h := NewSessionFork(SessionForkDeps{})
		assert.Equal(t, "session_fork", h.Name())
		assert.Equal(t, []pipeline.Point{pipeline.SessionFork}, h.Points())
	})
}
