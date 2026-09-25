package turn_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
)

func TestTurn_KindShape(t *testing.T) {
	k := turn.Kind{}
	assert.Equal(t, "turn", k.Name())
	assert.Equal(t, "turn-", k.IDPrefix())
	assert.True(t, k.Retention().EssentialWhileLive, "turn entries are essential while the scope is live")
}

func TestTurn_Registered(t *testing.T) {
	k, ok := memory.LookupKind("turn")
	require.True(t, ok, "turn Kind must self-register via init()")
	assert.Equal(t, "turn-", k.IDPrefix())
}
