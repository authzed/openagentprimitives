package toolchainaudit

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestKind_Metadata(t *testing.T) {
	k := Kind{}
	assert.Equal(t, "toolchain_audit", k.Name())
	assert.Equal(t, "tcaud-", k.IDPrefix())
	assert.True(t, k.Retention().AppendOnly,
		"a toolchain selection changes what code can execute in the sandbox; it is audit evidence")
	assert.Contains(t, k.IndexedFields(), "phase")
	assert.Contains(t, k.IndexedFields(), "setDigest")
}
