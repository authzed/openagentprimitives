package agentsession

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

func TestForkHost_Notify_PublishesToParentSubject(t *testing.T) {
	var gotNs, gotName, gotRequester, gotBody string
	called := 0
	h := newForkHost(forkHostDeps{
		ParentNs:   "ns",
		ParentName: "parent",
		Forker:     "user:bob",
		NoticePublish: func(_ context.Context, ns, name, requester, body string) error {
			called++
			gotNs, gotName, gotRequester, gotBody = ns, name, requester, body
			return nil
		},
	})
	err := h.Notify(context.Background(), pipeline.Notice{Notice: testNotice("denied"), ToRequester: true})
	require.NoError(t, err)
	assert.Equal(t, 1, called)
	assert.Equal(t, "ns", gotNs)
	assert.Equal(t, "parent", gotName)
	assert.Equal(t, "user:bob", gotRequester)
	assert.Equal(t, "denied — Send it again.", gotBody)
}

func TestForkHost_Notify_NilPublish_NoPanic(t *testing.T) {
	h := newForkHost(forkHostDeps{ParentNs: "ns", ParentName: "parent", Forker: "user:bob"})
	assert.NotPanics(t, func() {
		_ = h.Notify(context.Background(), pipeline.Notice{Notice: testNotice("x"), ToRequester: true})
	})
}

func TestForkHost_ApprovalUnsupported(t *testing.T) {
	h := newForkHost(forkHostDeps{})
	_, err := h.PublishApproval(context.Background(), pipeline.ApprovalAsk{Kind: "x"})
	require.Error(t, err)
	_, _, _, aerr := h.AwaitDecision(context.Background(), "rid", 0)
	require.Error(t, aerr)
}

func TestForkHost_Audit_SetStatus_Halt_NoError(t *testing.T) {
	h := newForkHost(forkHostDeps{ParentNs: "ns", ParentName: "parent"})
	require.NoError(t, h.Audit(context.Background(), []pipeline.AuditRecord{{Kind: "fork_decision"}}))
	require.NoError(t, h.SetStatus(context.Background(), pipeline.StatusUpdate{Text: "x"}))
	require.NoError(t, h.Halt(context.Background(), "reason"))
}

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
