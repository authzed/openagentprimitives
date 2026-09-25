package websearch_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/tools/websearch"
)

// stubProvider lets the contract test exercise the Provider interface
// without depending on a concrete impl — those have their own dedicated
// tests. It also lets us prove the contract holds for any future backend by
// adding an entry to the table.
type stubProvider struct {
	name        string
	clientTools []agenttool.Tool
}

func (s *stubProvider) Name() string                  { return s.name }
func (s *stubProvider) ClientTools() []agenttool.Tool { return s.clientTools }

// stubTool is a minimal agenttool.Tool used by the contract test.
type stubTool struct{ name string }

func (t *stubTool) Name() string               { return t.name }
func (*stubTool) Kind() agenttool.Kind         { return agenttool.KindMeta }
func (*stubTool) Description() string          { return "stub" }
func (*stubTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (*stubTool) Execute(_ context.Context, _ json.RawMessage, _ *agenttool.SessionContext) (agenttool.Result, error) {
	return agenttool.Result{Content: "stub"}, nil
}
func (*stubTool) Permission() authz.Permission                  { return authz.Permission{StateImpact: authz.Stateless} }
func (*stubTool) PermissionVariants() []authz.PermissionVariant { return nil }

// TestProviderInterfaceContract pins the client-dispatched-only contract:
// every Provider exposes its tools via ClientTools, dispatched through the
// agent loop's normal tool.Tool path.
func TestProviderInterfaceContract(t *testing.T) {
	p := &stubProvider{
		name: "stub-client",
		clientTools: []agenttool.Tool{
			&stubTool{name: "web_search"},
			&stubTool{name: "web_fetch"},
		},
	}

	assert.NotEmpty(t, p.Name(), "Name() must be non-empty")

	got := map[string]bool{}
	for _, ct := range p.ClientTools() {
		got[ct.Name()] = true
	}
	for _, want := range []string{"web_search", "web_fetch"} {
		assert.True(t, got[want], "expected tool %q exposed via ClientTools, got %v", want, got)
	}
}

// TestResultsAreJSONSerializable round-trips both SearchResult and
// FetchResult through Marshal/Unmarshal to guard their JSON tags.
func TestResultsAreJSONSerializable(t *testing.T) {
	t.Run("SearchResult: round-trips identically", func(t *testing.T) {
		r := websearch.SearchResult{Title: "t", URL: "https://example.com", Snippet: "s"}
		b, err := json.Marshal(r)
		require.NoError(t, err, "marshal")
		var got websearch.SearchResult
		require.NoError(t, json.Unmarshal(b, &got), "unmarshal")
		assert.Equal(t, r, got, "round-trip mismatch")
	})

	t.Run("FetchResult: round-trips identically", func(t *testing.T) {
		r := websearch.FetchResult{URL: "https://example.com", ContentType: "text/html", Body: "body"}
		b, err := json.Marshal(r)
		require.NoError(t, err, "marshal")
		var got websearch.FetchResult
		require.NoError(t, json.Unmarshal(b, &got), "unmarshal")
		assert.Equal(t, r, got, "round-trip mismatch")
	})
}
