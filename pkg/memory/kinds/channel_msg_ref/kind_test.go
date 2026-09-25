package channel_msg_ref_test

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/channel_msg_ref"
)

func TestKind_Shape(t *testing.T) {
	var k channel_msg_ref.Kind
	assert.Equal(t, "channel_msg_ref", k.Name())
	assert.Equal(t, "cmref-", k.IDPrefix())
	assert.True(t, k.Retention().EssentialWhileLive)
	assert.Equal(t, reflect.TypeOf(channel_msg_ref.Content{}), k.ContentSchema())
}

func TestKind_Registered(t *testing.T) {
	k, ok := memory.LookupKind("channel_msg_ref")
	assert.True(t, ok)
	if ok {
		assert.Equal(t, "channel_msg_ref", k.Name())
	}
}
