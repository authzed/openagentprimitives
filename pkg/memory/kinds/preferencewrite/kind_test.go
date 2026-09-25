package preferencewrite

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	memoryinmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

func TestPreferenceWrite_RecordAppendsSignableEntry(t *testing.T) {
	mem := memory.NewLocal(memoryinmem.NewBackend())
	scope, _ := memory.UserScope("YWxpY2U")
	ctx := memory.SystemContext(t.Context(), "test")

	require.NoError(t, Record(ctx, mem, scope, Content{
		Subject:        "YWxpY2U",
		ClassNamespace: "ns",
		ClassName:      "reviewbot",
		Key:            "notifications",
		Value:          json.RawMessage("false"),
		Via:            "app-home",
	}))
	// append-only: a second record appends a second distinct entry (random ID), not a conflict.
	require.NoError(t, Record(ctx, mem, scope, Content{
		Subject:        "YWxpY2U",
		ClassNamespace: "ns",
		ClassName:      "reviewbot",
		Key:            "notifications",
		Value:          json.RawMessage("true"),
		Via:            "app-home",
	}))

	got, err := List(ctx, mem, scope)
	require.NoError(t, err)
	require.Len(t, got, 2, "append-only: every write is its own entry, even when to the same key")
	assert.Equal(t, "YWxpY2U", got[0].Subject)
	assert.Equal(t, "ns", got[0].ClassNamespace)
	assert.Equal(t, "reviewbot", got[0].ClassName)
	assert.Equal(t, "notifications", got[0].Key)
	assert.Equal(t, json.RawMessage("false"), got[0].Value)
	assert.Equal(t, "app-home", got[0].Via)
	assert.Equal(t, json.RawMessage("true"), got[1].Value)
}

func TestPreferenceWrite_KindMetadata(t *testing.T) {
	k := Kind{}
	assert.Equal(t, "preference_write", k.Name())
	assert.Equal(t, memory.ComponentWritten, k.WriteAuthority())
	assert.True(t, k.Retention().AppendOnly)
}
