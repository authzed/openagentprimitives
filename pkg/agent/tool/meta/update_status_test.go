package meta_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
)

func TestUpdateStatusTool_Name(t *testing.T) {
	ut := meta.NewUpdateStatus(meta.UpdateStatusConfig{})
	assert.Equal(t, "update_status", ut.Name())
}

// TestUpdateStatusTool_NoNATSPublish_ResultIsTrusted is the Minor-gap
// regression test the reviewer asked for: update_status's no-NATS guard
// returns a hand-written Result (not routed through tool.ArgParseError), so
// it needs its own coverage that the result stays Trusted — the
// meta-tool-untrusted-by-default default would otherwise route framework
// text through content-guard inspection.
func TestUpdateStatusTool_NoNATSPublish_ResultIsTrusted(t *testing.T) {
	ut := meta.NewUpdateStatus(meta.UpdateStatusConfig{}) // NATSPublish left nil

	res, err := ut.Execute(context.Background(), json.RawMessage(`{"text":"working","short":"working"}`), nil)
	require.Error(t, err, "an unset NATSPublish must surface as a Go error so dispatchToolUses wraps it")
	assert.True(t, res.Trusted, "update_status's no-NATS result is framework-generated and must be Trusted")
}

// TestUpdateStatusTool_MalformedArgs_ResultIsTrusted exercises the shared
// tool.ArgParseError path directly through the real tool's Execute.
func TestUpdateStatusTool_MalformedArgs_ResultIsTrusted(t *testing.T) {
	pub := &fakeNATSPublish{}
	ut := meta.NewUpdateStatus(meta.UpdateStatusConfig{NATSPublish: pub.publish})

	res, err := ut.Execute(context.Background(), json.RawMessage(`{not valid json`), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.True(t, res.Trusted, "a malformed-args result must be Trusted (routes through tool.ArgParseError)")
}

// TestUpdateStatusTool_EmptyText_ResultIsTrusted covers the hand-written
// validation-error branches (missing text/short), which also must stay
// Trusted.
func TestUpdateStatusTool_EmptyText_ResultIsTrusted(t *testing.T) {
	pub := &fakeNATSPublish{}
	ut := meta.NewUpdateStatus(meta.UpdateStatusConfig{NATSPublish: pub.publish})

	res, err := ut.Execute(context.Background(), json.RawMessage(`{"text":"","short":"x"}`), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content, "text")
	assert.True(t, res.Trusted, "update_status's empty-text validation result must be Trusted")
}
