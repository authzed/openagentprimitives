package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"

	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// TestTimeoutPolicyMatchesLifecycleTable locks the runner's stamped OnTimeout to
// the single lifecycle decisionParams authority — if the table ever marks an
// executor-path kind fail-closed, this fails until the stamping agrees.
func TestTimeoutPolicyMatchesLifecycleTable(t *testing.T) {
	cases := []struct {
		askKind string
		dk      lifecyclecore.DecisionKind
	}{
		{"tool_call", lifecyclecore.DecisionToolCall},
		{"leakage_share", lifecyclecore.DecisionLeakageShare},
		{"content_inspection", lifecyclecore.DecisionContentInspect},
	}
	for _, tc := range cases {
		t.Run(tc.askKind, func(t *testing.T) {
			want := pipeline.TimeoutDeny
			if lifecyclecore.FailsClosedOnTimeout(tc.dk) {
				want = pipeline.TimeoutHalt
			}
			assert.Equal(t, want, timeoutPolicyFor(tc.askKind))
		})
	}
}
