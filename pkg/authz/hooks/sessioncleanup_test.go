package hooks_test

import (
	"context"
	"errors"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// findAudit returns the first AuditRecord of the given kind, or nil.
func findAudit(recs []pipeline.AuditRecord, kind string) *pipeline.AuditRecord {
	for i := range recs {
		if recs[i].Kind == kind {
			return &recs[i]
		}
	}
	return nil
}

func TestSessionCleanup_EmitsSessionEndAudit(t *testing.T) {
	cases := []struct {
		name   string
		reason string
	}{
		{name: "completed: session_end audit with reason=completed", reason: "completed"},
		{name: "idle: session_end audit with reason=idle", reason: "idle"},
		{name: "failed: session_end audit with reason=failed", reason: "failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := hooks.NewSessionCleanup(hooks.SessionCleanupDeps{})
			dec := h.Eval(context.Background(), pipeline.Input{
				Point:   pipeline.SessionEnd,
				Session: pipeline.SessionRef{Namespace: "default", Name: "s1"},
				End:     &pipeline.SessionEndInfo{Reason: tc.reason},
			})

			assert.Equal(t, pipeline.Allow, dec.Verdict, "cleanup never gates the terminal write")
			rec := findAudit(dec.Audit, "session_end")
			require.NotNil(t, rec, "must emit a session_end audit record")
			assert.Equal(t, tc.reason, rec.Fields["reason"], "audit carries the end reason")
			assert.Equal(t, "default/s1", rec.Fields["session"], "audit carries the session ref")
		})
	}
}

func TestSessionCleanup_RemoveExternalState_InvokedOnce(t *testing.T) {
	calls := 0
	h := hooks.NewSessionCleanup(hooks.SessionCleanupDeps{
		RemoveExternalState: func(context.Context) error { calls++; return nil },
	})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point:   pipeline.SessionEnd,
		Session: pipeline.SessionRef{Namespace: "default", Name: "s1"},
		End:     &pipeline.SessionEndInfo{Reason: "completed"},
	})
	assert.Equal(t, pipeline.Allow, dec.Verdict)
	assert.Equal(t, 1, calls, "RemoveExternalState invoked exactly once when wired")
}

func TestSessionCleanup_RemoveExternalState_NilIsNoOp(t *testing.T) {
	h := hooks.NewSessionCleanup(hooks.SessionCleanupDeps{}) // nil RemoveExternalState
	assert.NotPanics(t, func() {
		dec := h.Eval(context.Background(), pipeline.Input{
			Point:   pipeline.SessionEnd,
			Session: pipeline.SessionRef{Namespace: "default", Name: "s1"},
			End:     &pipeline.SessionEndInfo{Reason: "idle"},
		})
		assert.Equal(t, pipeline.Allow, dec.Verdict)
	}, "nil RemoveExternalState must be a no-op, not a panic")
}

func TestSessionCleanup_RemoveExternalStateError_DoesNotAbort(t *testing.T) {
	h := hooks.NewSessionCleanup(hooks.SessionCleanupDeps{
		RemoveExternalState: func(context.Context) error { return errors.New("boom") },
	})
	dec := h.Eval(context.Background(), pipeline.Input{
		Point:   pipeline.SessionEnd,
		Session: pipeline.SessionRef{Namespace: "default", Name: "s1"},
		End:     &pipeline.SessionEndInfo{Reason: "failed"},
	})
	// Cleanup never turns a removal error into a Deny/Halt — the terminal write
	// must proceed; the error is logged + swallowed.
	assert.Equal(t, pipeline.Allow, dec.Verdict)
	assert.NotNil(t, findAudit(dec.Audit, "session_end"), "audit still emitted after a removal error")
}

func TestSessionCleanup_NameAndPoints(t *testing.T) {
	h := hooks.NewSessionCleanup(hooks.SessionCleanupDeps{})
	assert.Equal(t, "session_cleanup", h.Name())
	assert.Equal(t, []pipeline.Point{pipeline.SessionEnd}, h.Points())
}
