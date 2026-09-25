package tools_test

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/llmagent/tools"
)

func TestPromptUserText(t *testing.T) {
	old := tools.PromptUser
	tools.PromptUser = func(ctx context.Context, message string, kind string, choices []string, def string, stdin io.Reader, stdout io.Writer) (string, error) {
		return "answered", nil
	}
	t.Cleanup(func() { tools.PromptUser = old })

	out, err := tools.PromptUserRun(context.Background(),
		json.RawMessage(`{"message":"go","kind":"text"}`),
		strings.NewReader(""), io.Discard)
	require.NoError(t, err, "PromptUserRun")
	assert.Equal(t, "answered", out, "PromptUserRun output")
}

func TestPromptUserChoiceRequiresChoices(t *testing.T) {
	_, err := tools.PromptUserRun(context.Background(),
		json.RawMessage(`{"message":"pick","kind":"choice"}`),
		strings.NewReader(""), io.Discard)
	require.Error(t, err, "expected error on missing choices")
}
