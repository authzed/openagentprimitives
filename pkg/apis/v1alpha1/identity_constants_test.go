package v1alpha1_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestIdentityModeConstants pins the string values of the ask|dynamic
// identityMode + AwaitingIdentityChoice phase constants. Later tasks
// (lifecycle, operator, runner, Slack) all consume these by name — the
// literal values are the CRD's persisted representation and must not
// drift silently.
func TestIdentityModeConstants(t *testing.T) {
	assert.Equal(t, "ask", v1alpha1.IdentityModeAsk)
	assert.Equal(t, "dynamic", v1alpha1.IdentityModeDynamic)
	assert.Equal(t, "AwaitingIdentityChoice", v1alpha1.AgentSessionPhaseAwaitingIdentityChoice)
}
