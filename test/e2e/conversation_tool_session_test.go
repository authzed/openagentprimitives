//go:build e2e

package e2e

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func TestToolSessionPredicates(t *testing.T) {
	out := channelevents.ToolSessionDeltaPayload{
		ToolCallRef: "alice-1-x",
		Data:        []byte("hello world"),
	}
	term := channelevents.ToolSessionDeltaPayload{
		ToolCallRef: "alice-1-x",
		Terminal:    true,
		ExitReason:  "completed",
	}

	assert.True(t, ToolSessionContains("hello")(out))
	assert.True(t, ToolSessionContains("hello", "world")(out))
	assert.False(t, ToolSessionContains("nope")(out))
	assert.False(t, ToolSessionContains("hello")(term), "Terminal must not match content predicates")

	assert.True(t, Terminal("")(term))
	assert.True(t, Terminal("completed")(term))
	assert.False(t, Terminal("idle")(term))
	assert.False(t, Terminal("")(out))

	assert.True(t, ToolCallRef("alice-1-x")(out))
	assert.False(t, ToolCallRef("other")(out))

	assert.True(t, And(ToolCallRef("alice-1-x"), Terminal("completed"))(term))
	assert.False(t, And(ToolCallRef("alice-1-x"), Terminal("idle"))(term))
}
