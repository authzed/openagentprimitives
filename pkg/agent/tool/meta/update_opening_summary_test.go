package meta_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/openingsummary"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	channelevents "github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func newSummarySess(t *testing.T) *tool.SessionContext {
	t.Helper()
	return &tool.SessionContext{Namespace: "default", Name: "gh-sess", State: state.NewRegistry(state.Deps{})}
}

func TestUpdateOpeningSummary_Name(t *testing.T) {
	assert.Equal(t, "update_opening_summary", meta.NewUpdateOpeningSummary(meta.UpdateOpeningSummaryConfig{}).Name())
}

func TestUpdateOpeningSummary_WritesBodyToStore(t *testing.T) {
	sess := newSummarySess(t)
	tl := meta.NewUpdateOpeningSummary(meta.UpdateOpeningSummaryConfig{})
	res, err := tl.Execute(context.Background(), json.RawMessage(`{"body":"2 findings"}`), sess)
	require.NoError(t, err)
	require.False(t, res.IsError, "content: %s", res.Content)

	store, ok := openingsummary.TryFrom(sess)
	require.True(t, ok)
	assert.Equal(t, "2 findings", store.Body())
}

func TestUpdateOpeningSummary_EmptyBodyClears(t *testing.T) {
	sess := newSummarySess(t)
	tl := meta.NewUpdateOpeningSummary(meta.UpdateOpeningSummaryConfig{})
	_, _ = tl.Execute(context.Background(), json.RawMessage(`{"body":"x"}`), sess)
	res, err := tl.Execute(context.Background(), json.RawMessage(`{"body":""}`), sess)
	require.NoError(t, err)
	require.False(t, res.IsError)
	store, _ := openingsummary.TryFrom(sess)
	assert.Equal(t, "", store.Body(), "empty body clears the enrichment")
}

func TestUpdateOpeningSummary_MalformedArgs_Trusted(t *testing.T) {
	res, err := meta.NewUpdateOpeningSummary(meta.UpdateOpeningSummaryConfig{}).
		Execute(context.Background(), json.RawMessage(`{not json`), newSummarySess(t))
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.True(t, res.Trusted)
}

func TestUpdateOpeningSummary_LeakageGateBlocks_NoStoreWrite(t *testing.T) {
	sess := newSummarySess(t)
	tl := meta.NewUpdateOpeningSummary(meta.UpdateOpeningSummaryConfig{
		LeakageGate: func(context.Context, *tool.SessionContext, string, []channelevents.AttachmentRef) error {
			return errors.New("blocked")
		},
	})
	res, err := tl.Execute(context.Background(), json.RawMessage(`{"body":"secret"}`), sess)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	store, _ := openingsummary.TryFrom(sess)
	assert.Equal(t, "", store.Body(), "a gate-blocked body is never recorded")
}
