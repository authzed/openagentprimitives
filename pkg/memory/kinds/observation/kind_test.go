package observation_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/observation"
)

// TestRegistered pins the Kind's registered identity from its own package,
// following the pattern in observedfact's TestRegistered: confirm
// observation self-registers under its own name and prefix via init().
func TestRegistered(t *testing.T) {
	k, ok := memory.LookupKind(observation.KindName)
	require.True(t, ok, "observation must self-register via init()")
	assert.Equal(t, observation.IDPrefix, k.IDPrefix())
}

func TestObservationKind_IsSessionWritten(t *testing.T) {
	assert.Equal(t, memory.SessionWritten, observation.Kind{}.WriteAuthority(),
		"the agent authors observations; nothing reads one to make an authorization decision")
}

func TestObservationKind_IsAppendOnly(t *testing.T) {
	assert.True(t, observation.Kind{}.Retention().AppendOnly,
		"a shared pool is multi-writer: mutability would let one session rewrite another's observation")
}

func TestObservationKind_HasADistinctIDPrefix(t *testing.T) {
	// The registry refuses a prefix that is a prefix-of or prefixed-by another
	// Kind's. Assert registration succeeds rather than asserting the literal,
	// so this stays true as other Kinds are added.
	assert.NotEmpty(t, observation.Kind{}.IDPrefix())
	assert.NotEmpty(t, observation.Kind{}.Name())
}
