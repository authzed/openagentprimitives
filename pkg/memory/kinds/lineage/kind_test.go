package lineage_test

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lineage"
)

func TestKind_Shape(t *testing.T) {
	var k lineage.Kind
	assert.Equal(t, "lineage", k.Name())
	assert.Equal(t, "lineage-", k.IDPrefix())
	assert.True(t, k.Retention().EssentialWhileLive, "lineage edges must survive while session live")
	assert.Equal(t, reflect.TypeOf(lineage.Content{}), k.ContentSchema())
}

func TestKind_RegisteredInRegistry(t *testing.T) {
	k, ok := memory.LookupKind("lineage")
	assert.True(t, ok, "lineage kind should auto-register via init()")
	if ok {
		assert.Equal(t, "lineage", k.Name())
	}
}
