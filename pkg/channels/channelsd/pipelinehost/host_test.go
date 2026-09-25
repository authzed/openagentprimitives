package pipelinehost

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// TestHost_EffectsAreBestEffortNoError verifies Notify/SetStatus/Halt/Audit are
// thin, log-only, and never error — channelsd's interact gate produces only
// Allow/Deny, no notices, status, or audit through the host.
func TestHost_EffectsAreBestEffortNoError(t *testing.T) {
	h := New(Deps{Session: SessionRef{Namespace: "ns", Name: "a"}})
	ctx := context.Background()
	assert.NoError(t, h.Notify(ctx, pipeline.Notice{Notice: testNotice("x")}))
	assert.NoError(t, h.SetStatus(ctx, pipeline.StatusUpdate{Text: "y"}))
	assert.NoError(t, h.Halt(ctx, "z"))
	assert.NoError(t, h.Audit(ctx, []pipeline.AuditRecord{{Kind: "k"}}))
}

// TestHost_ApprovalUnsupported verifies the interact host rejects approval —
// interact never asks for approval, so PublishApproval/AwaitDecision return
// ErrApprovalUnsupported (fail-closed: the executor turns the publish error
// into Halt rather than silently allowing).
func TestHost_ApprovalUnsupported(t *testing.T) {
	h := New(Deps{Session: SessionRef{Namespace: "ns", Name: "a"}})
	ctx := context.Background()

	_, err := h.PublishApproval(ctx, pipeline.ApprovalAsk{Kind: "anything"})
	require.ErrorIs(t, err, ErrApprovalUnsupported)

	_, _, _, err = h.AwaitDecision(ctx, "req-1", time.Minute)
	require.ErrorIs(t, err, ErrApprovalUnsupported)
}

var _ pipeline.Host = New(Deps{})

// testNotice builds a real notice for a test that only cares that SOME
// user-facing message was produced. It uses a registered category so the
// notice is valid end-to-end rather than a hand-built struct that could drift
// from what production can actually construct.
func testNotice(lead string) *notice.Notice {
	return notice.New(categories.InternalError, notice.Args{
		Lead:     lead,
		NextStep: "Send it again.",
		Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
	})
}
