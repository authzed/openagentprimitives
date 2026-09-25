package runner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/openingsummary"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestRecomputePinnedMessage_OnlyWritesOnChange(t *testing.T) {
	sess := &tool.SessionContext{Namespace: "default", Name: "gh-sess", State: state.NewRegistry(state.Deps{})}
	os, _ := openingsummary.TryFrom(sess)

	var writes []*v1alpha1.PinnedMessageStatus
	patch := func(_ context.Context, p *v1alpha1.PinnedMessageStatus) error { writes = append(writes, p); return nil }

	// hasOpening = true; not concluded; empty body -> in_progress, written once.
	require.NoError(t, recomputePinnedMessage(context.Background(), sess, true, patch))
	require.NoError(t, recomputePinnedMessage(context.Background(), sess, true, patch))
	require.Len(t, writes, 1, "identical state writes once")
	assert.Equal(t, v1alpha1.OpeningBadgeInProgress, writes[0].Badge)

	// Body change -> a second write.
	require.NoError(t, os.SetBody(context.Background(), "1 finding"))
	require.NoError(t, recomputePinnedMessage(context.Background(), sess, true, patch))
	require.Len(t, writes, 2)
	assert.Equal(t, "1 finding", writes[1].Body)
}
