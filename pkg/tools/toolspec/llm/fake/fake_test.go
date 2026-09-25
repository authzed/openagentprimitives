package fake

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/llm"
)

func TestFake_SelectToolkit_Hit(t *testing.T) {
	p := &Provider{
		Select: map[string]*llm.SelectResponse{
			"gh read-only": {ToolkitName: "gh", Reasoning: "matches GitHub"},
		},
	}
	got, err := p.SelectToolkit(context.Background(), llm.SelectRequest{Intent: "gh read-only"})
	require.NoError(t, err, "SelectToolkit")
	assert.Equal(t, "gh", got.ToolkitName, "ToolkitName")
}

func TestFake_SelectToolkit_Miss(t *testing.T) {
	p := &Provider{Select: map[string]*llm.SelectResponse{}}
	_, err := p.SelectToolkit(context.Background(), llm.SelectRequest{Intent: "unknown"})
	require.Error(t, err, "expected miss error")
	assert.ErrorContains(t, err, "fake: no Select response", "error message")
}

func TestFake_GenerateSpec_ConsumedInOrder(t *testing.T) {
	p := &Provider{
		Generate: []*llm.GenerateResponse{
			{SpecYAML: "first"},
			{SpecYAML: "second"},
		},
	}
	one, err := p.GenerateSpec(context.Background(), llm.GenerateRequest{})
	require.NoError(t, err, "first call")
	assert.Equal(t, "first", one.SpecYAML, "first response")
	two, err := p.GenerateSpec(context.Background(), llm.GenerateRequest{})
	require.NoError(t, err, "second call")
	assert.Equal(t, "second", two.SpecYAML, "second response")
	_, err = p.GenerateSpec(context.Background(), llm.GenerateRequest{})
	assert.Error(t, err, "third call should exhaust")
}

func TestFake_GenerateTestCases(t *testing.T) {
	p := &Provider{
		Tests: map[string]*llm.TestResponse{
			"intent-key": {TestCases: []llm.TestCase{{Intent: "x", Argv: []string{"a"}, ExpectAllow: true}}},
		},
	}
	got, err := p.GenerateTestCases(context.Background(), llm.TestRequest{Intent: "intent-key"})
	require.NoError(t, err, "GenerateTestCases")
	require.Len(t, got.TestCases, 1, "TestCases length")
	assert.Equal(t, "x", got.TestCases[0].Intent, "TestCases[0].Intent")
}

func TestFake_RefineSpec_ConsumedInOrder(t *testing.T) {
	p := &Provider{
		Refine: []*llm.GenerateResponse{{SpecYAML: "refined-1"}},
	}
	got, err := p.RefineSpec(context.Background(), llm.RefineRequest{})
	require.NoError(t, err, "first call")
	assert.Equal(t, "refined-1", got.SpecYAML, "SpecYAML")
	_, err = p.RefineSpec(context.Background(), llm.RefineRequest{})
	assert.Error(t, err, "second call should exhaust")
}
