package oap

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestManifestHandlerConfigRoundTripAndVersionGate(t *testing.T) {
	raw := []byte(`oapFormatVersion: "2"
agent:
  name: synthetic-agent
  version: "1"
requires:
  channels:
    - name: synthetic-input
      kind: github
      role: input
      config:
        trigger:
          event: workflow_run
          actions: [completed]
          readiness: all_completed
`)
	m, err := ParseManifest(raw)
	require.NoError(t, err)
	require.NoError(t, m.Validate())
	require.JSONEq(t, `{"trigger":{"event":"workflow_run","actions":["completed"],"readiness":"all_completed"}}`, string(m.Requires.Channels[0].Config))
	m.OapFormatVersion = "1"
	require.ErrorContains(t, m.Validate(), "requires oapFormatVersion 2")
}
