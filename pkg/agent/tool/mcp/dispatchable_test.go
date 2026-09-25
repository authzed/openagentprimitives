package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// stubTool is a minimal agenttool.Tool that only has to be identifiable by
// name — Dispatchable does not call into a tool, it decides which ones a
// caller must not skip.
type stubTool struct{ name string }

func (s stubTool) Name() string                                  { return s.name }
func (s stubTool) Description() string                           { return "" }
func (s stubTool) InputSchema() json.RawMessage                  { return nil }
func (s stubTool) Kind() agenttool.Kind                          { return "mcp" }
func (s stubTool) Permission() authz.Permission                  { return authz.Permission{} }
func (s stubTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (s stubTool) Execute(context.Context, json.RawMessage, *agenttool.SessionContext) (agenttool.Result, error) {
	return agenttool.Result{}, nil
}

// The invariant that failed in production. An app-only tool is reachable —
// an agent-UI data binding calls it directly — so anything a caller wires for
// "a call about to go upstream" has to cover it. Wiring only LLMTools sent
// every app-only call out with no Authorization header, and the 401 that came
// back was indistinguishable from a rejected credential.
func TestDispatchableCoversAppVisibleToolsNotJustTheLLMsOwn(t *testing.T) {
	cases := []struct {
		name string
		res  SynthesizeResult
		want []string
	}{
		{
			name: "app-only tools are included: they dispatch upstream like any other",
			res: SynthesizeResult{
				LLMTools: []agenttool.Tool{stubTool{"crm_list_records"}},
				AppTools: []agenttool.Tool{stubTool{"crm_search_records"}},
			},
			want: []string{"crm_list_records", "crm_search_records"},
		},
		{
			// The exact shape of the outage: a server whose ONLY tool is
			// app-visible. Under the old wiring this session had nothing
			// authenticated at all, yet started cleanly.
			name: "a server with only app-visible tools still yields a set to wire",
			res: SynthesizeResult{
				AppTools: []agenttool.Tool{stubTool{"crm_search_records"}},
			},
			want: []string{"crm_search_records"},
		},
		{
			name: "no app tools: unchanged from the LLM-only case",
			res: SynthesizeResult{
				LLMTools: []agenttool.Tool{stubTool{"crm_list_records"}},
			},
			want: []string{"crm_list_records"},
		},
		{
			name: "nothing synthesized: empty, not nil-panicking",
			res:  SynthesizeResult{},
			want: []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := make([]string, 0, len(tc.res.Dispatchable()))
			for _, tl := range tc.res.Dispatchable() {
				got = append(got, tl.Name())
			}
			assert.ElementsMatch(t, tc.want, got)
		})
	}
}

// Dispatchable must not alias either input slice. The caller iterates it while
// holding both lists, and a returned slice sharing LLMTools' backing array
// would let an append here overwrite a tool the model is about to be offered.
func TestDispatchableDoesNotAliasItsInputs(t *testing.T) {
	llm := make([]agenttool.Tool, 1, 4) // spare capacity: the aliasing trap
	llm[0] = stubTool{"crm_list_records"}
	res := SynthesizeResult{LLMTools: llm, AppTools: []agenttool.Tool{stubTool{"crm_search_records"}}}

	out := res.Dispatchable()
	require.Len(t, out, 2)

	out[0] = stubTool{"mutated"}
	assert.Equal(t, "crm_list_records", res.LLMTools[0].Name(), "writing to the result must not reach LLMTools")
	assert.Equal(t, "crm_search_records", res.AppTools[0].Name(), "nor AppTools")
}
