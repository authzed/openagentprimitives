package lifecycle

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFailsClosedOnTimeout(t *testing.T) {
	assert.True(t, FailsClosedOnTimeout(DecisionScopeReview), "scope_review is fail-closed on timeout")
	assert.False(t, FailsClosedOnTimeout(DecisionToolCall))
	assert.False(t, FailsClosedOnTimeout(DecisionLeakageShare))
	assert.False(t, FailsClosedOnTimeout(DecisionContentInspect))
	assert.False(t, FailsClosedOnTimeout(DecisionJoin))
}
