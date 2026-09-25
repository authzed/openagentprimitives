package observedfact_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/observedfact"
)

// TestRegistered pins the Kind's registered identity from its OWN package.
// The cross-Kind provenance tripwire (envelope_fact vs observed_fact
// WriteAuthority) lives in envelopefact's test, which imports both; this one
// confirms observed_fact self-registers under its own name and prefix
// independent of that cross-check.
func TestRegistered(t *testing.T) {
	k, ok := memory.LookupKind(observedfact.KindName)
	require.True(t, ok, "observed_fact must self-register via init()")
	assert.Equal(t, observedfact.IDPrefix, k.IDPrefix())
	assert.Equal(t, memory.SessionWritten, observedfact.Kind{}.WriteAuthority(),
		"the runner records facts derived from its own tool results")
}

func TestIndexedFieldsMatchContentTags(t *testing.T) {
	// IndexedFields must name Content's json tags (the stored keys), not its Go
	// field names, or a FieldFilter.Path built from this list matches nothing.
	assert.ElementsMatch(t, []string{"resourceType", "resourceID"}, observedfact.Kind{}.IndexedFields())
}
