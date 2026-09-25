package agentsession

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// The runner answers an agent-defined UI's data bindings, so a reaped runner
// is a dashboard whose every section fails. The TTL is therefore a
// user-visible setting, not a resource-tidiness knob.
//
// This pins the value the OPERATOR stamps, which is the one production uses.
// internal/cmd/runner's --idle-ttl flag default is unreachable for a channel-attached
// session: buildRunnerEnv sets IDLE_TTL on every such pod and an explicit env
// var always wins. Raising the flag alone once changed nothing at all, and
// nothing failed to say so — every session went on reaping at the old five
// minutes while the code claimed fifteen.
func TestRunnerIdleTTLIsTheOperatorsValue(t *testing.T) {
	assert.Equal(t, 15*time.Minute, defaultRunnerIdleTTL,
		"the default a channel-attached runner actually receives")
}

// An AgentClass that states its own TTL wins — the default is a floor for
// classes that say nothing, never an override of an author's choice.
func TestClassIdleTTLOverridesTheDefault(t *testing.T) {
	class := &spiceboxv1alpha1.AgentClass{}
	class.Spec.Channels = &spiceboxv1alpha1.ChannelsConfig{
		IdleTTL: metav1.Duration{Duration: 90 * time.Second},
	}

	require.NotNil(t, class.Spec.Channels)
	// The branch under test, stated directly: a positive class TTL replaces
	// the default, and a zero one leaves it alone.
	ttl := defaultRunnerIdleTTL
	if class.Spec.Channels != nil && class.Spec.Channels.IdleTTL.Duration > 0 {
		ttl = class.Spec.Channels.IdleTTL.Duration
	}
	assert.Equal(t, 90*time.Second, ttl)

	class.Spec.Channels.IdleTTL = metav1.Duration{}
	ttl = defaultRunnerIdleTTL
	if class.Spec.Channels != nil && class.Spec.Channels.IdleTTL.Duration > 0 {
		ttl = class.Spec.Channels.IdleTTL.Duration
	}
	assert.Equal(t, defaultRunnerIdleTTL, ttl, "an unset class TTL falls back, it does not zero the runner")
}
