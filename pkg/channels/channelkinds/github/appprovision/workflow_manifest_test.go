package appprovision

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestWorkflowManifestPermissionsAreOptIn(t *testing.T) {
	for _, workflow := range []bool{false, true} {
		raw, err := BuildManifest(Params{Name: "synthetic", ExternalBaseURL: "https://events.example.org", WorkflowRun: workflow})
		require.NoError(t, err)
		var doc manifestDoc
		require.NoError(t, json.Unmarshal([]byte(raw), &doc))
		if workflow {
			require.Equal(t, []string{"workflow_run"}, doc.DefaultEvents)
			require.Equal(t, "read", doc.DefaultPerms["actions"])
		} else {
			require.Equal(t, []string{"pull_request"}, doc.DefaultEvents)
			require.NotContains(t, doc.DefaultPerms, "actions")
		}
		require.Equal(t, "write", doc.DefaultPerms["checks"])
	}
}
