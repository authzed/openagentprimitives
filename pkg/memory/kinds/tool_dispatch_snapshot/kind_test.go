package tool_dispatch_snapshot_test

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/tool_dispatch_snapshot"
)

func TestKind_Shape(t *testing.T) {
	var k tool_dispatch_snapshot.Kind
	assert.Equal(t, "tool_dispatch_snapshot", k.Name())
	assert.Equal(t, "tds-", k.IDPrefix())
	assert.True(t, k.Retention().EssentialWhileLive)
	assert.Equal(t, reflect.TypeOf(tool_dispatch_snapshot.Content{}), k.ContentSchema())
}

func TestKind_Registered(t *testing.T) {
	k, ok := memory.LookupKind("tool_dispatch_snapshot")
	assert.True(t, ok)
	if ok {
		assert.Equal(t, "tool_dispatch_snapshot", k.Name())
	}
}
