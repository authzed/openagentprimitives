package pipeline_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

func TestIsTimeout(t *testing.T) {
	assert.True(t, pipeline.IsTimeout(context.DeadlineExceeded), "raw deadline")
	assert.True(t, pipeline.IsTimeout(context.Canceled), "raw cancel")
	assert.True(t, pipeline.IsTimeout(fmt.Errorf("await: %w", context.DeadlineExceeded)), "wrapped deadline")
	assert.False(t, pipeline.IsTimeout(nil), "nil is not a timeout")
	assert.False(t, pipeline.IsTimeout(assert.AnError), "arbitrary error is not a timeout")
}

func TestTimeoutPolicyZeroValueIsDeny(t *testing.T) {
	var p pipeline.TimeoutPolicy
	assert.Equal(t, pipeline.TimeoutDeny, p, "zero value must be the safe (non-crashing) default")
}
