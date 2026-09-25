package tool_test

import (
	"context"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/stretchr/testify/assert"
)

type fakeCancellable struct{ tool.Tool }

func (fakeCancellable) Cancel(context.Context) error { return nil }

func TestCancellable_TypeAssertion(t *testing.T) {
	var interruptible tool.Tool = fakeCancellable{}
	_, ok := interruptible.(tool.Cancellable)
	assert.True(t, ok, "a tool with Cancel(ctx) satisfies tool.Cancellable")

	// A bare Tool (no Cancel) does not.
	var plain tool.Tool = noCancelTool{}
	_, ok = plain.(tool.Cancellable)
	assert.False(t, ok, "a tool without Cancel does not satisfy tool.Cancellable")
}

type noCancelTool struct{ tool.Tool }
