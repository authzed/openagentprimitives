package derivevalidator_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/derivevalidator"
	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta/capability"
)

type stubProvider struct{ reply string }

func (stubProvider) Name() string                            { return "stub" }
func (stubProvider) SupportedFromEnv() bool                  { return true }
func (stubProvider) Pricing(string) (llm.ModelPricing, bool) { return llm.ModelPricing{}, false }
func (stubProvider) Capabilities(string) llm.CapabilitySet   { return llm.CapabilitySet{} }
func (stubProvider) NativeInputMIMEs(string) llm.MIMESet     { return llm.MIMESet{} }
func (s stubProvider) Send(context.Context, llm.Request) (llm.Response, error) {
	return llm.Response{Content: []llm.ContentBlock{{Type: "text", Text: s.reply}}}, nil
}

func TestValidator(t *testing.T) {
	sources := []capability.DeriveSource{{TagID: "pt_1", Content: "Q3 revenue was $4.2M"}}

	// Judge approves a faithful transformation.
	v := derivevalidator.New(stubProvider{reply: `{"result": true, "reasoning": "supported by the source"}`}, "judge")
	ok, _, err := v.ValidateDerivation(context.Background(), sources, "Revenue reached $4.2M")
	require.NoError(t, err)
	assert.True(t, ok)

	// Judge rejects content that smuggles a fact not in the source (fences tolerated).
	v = derivevalidator.New(stubProvider{reply: "```json\n{\"result\": false, \"reasoning\": \"introduces the merger, not in source\"}\n```"}, "judge")
	ok, reason, err := v.ValidateDerivation(context.Background(), sources, "Revenue $4.2M; the merger closes Friday")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Contains(t, reason, "merger")

	// Unparseable judge response → fail closed (invalid + error).
	v = derivevalidator.New(stubProvider{reply: "not json at all"}, "judge")
	ok, _, err = v.ValidateDerivation(context.Background(), sources, "x")
	assert.Error(t, err)
	assert.False(t, ok)

	// Unconfigured (nil provider) → fail closed.
	v = derivevalidator.New(nil, "")
	ok, _, err = v.ValidateDerivation(context.Background(), sources, "x")
	assert.Error(t, err)
	assert.False(t, ok)
}
