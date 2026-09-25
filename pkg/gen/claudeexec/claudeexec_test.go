package claudeexec

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestArgs(t *testing.T) {
	args := Args("opus", "hello prompt")
	joined := strings.Join(args, " ")
	assert.Contains(t, joined, "-p")
	assert.Contains(t, joined, "--permission-mode acceptEdits")
	assert.Contains(t, joined, "--model opus")
	assert.Equal(t, "hello prompt", args[len(args)-1], "prompt is the final arg")
}
