package toolsession_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/toolsession"
)

func TestToolSession_KindShape(t *testing.T) {
	k := toolsession.Kind{}
	assert.Equal(t, "tool_session", k.Name())
	assert.Equal(t, "toolsess-", k.IDPrefix())
	assert.Equal(t, 2000, k.Retention().SoftCapPerScope, "tool_session entries are FIFO-capped per scope")
}

func TestToolSession_Registered(t *testing.T) {
	k, ok := memory.LookupKind("tool_session")
	require.True(t, ok, "tool_session Kind must self-register via init()")
	assert.Equal(t, "toolsess-", k.IDPrefix())
}
