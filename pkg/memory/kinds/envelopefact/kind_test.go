package envelopefact_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/envelopefact"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/observedfact"
)

// TestWriteAuthoritySplitIsTheProvenanceBoundary pins the reason these are two
// Kinds and not one with a `provenance` field. If envelope_fact ever becomes
// SessionWritten, a compromised runner can mint a fact that claims to have come
// from a signed provider payload — forging the trust grade the whole design
// rests on. This test is the tripwire.
func TestWriteAuthoritySplitIsTheProvenanceBoundary(t *testing.T) {
	assert.Equal(t, memory.ComponentWritten, envelopefact.Kind{}.WriteAuthority(),
		"envelope facts come from a verified delivery; a session must never author one")
	assert.Equal(t, memory.SessionWritten, observedfact.Kind{}.WriteAuthority(),
		"observed facts are written by the runner from its own tool results")
}

func TestKindsAreAppendOnlyAndDistinct(t *testing.T) {
	assert.True(t, envelopefact.Kind{}.Retention().AppendOnly,
		"a session must not be able to rewrite the facts that govern it")
	assert.True(t, observedfact.Kind{}.Retention().AppendOnly)

	assert.NotEqual(t, envelopefact.Kind{}.Name(), observedfact.Kind{}.Name())
	assert.NotEqual(t, envelopefact.Kind{}.IDPrefix(), observedfact.Kind{}.IDPrefix())
}
