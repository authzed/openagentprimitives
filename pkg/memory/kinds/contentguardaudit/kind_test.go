package contentguardaudit

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestKind_Metadata(t *testing.T) {
	k := Kind{}
	assert.Equal(t, "contentguard_audit", k.Name())
	assert.Equal(t, "cgaud-", k.IDPrefix())
	assert.True(t, k.Retention().AppendOnly)
	assert.Contains(t, k.IndexedFields(), "action")
}
