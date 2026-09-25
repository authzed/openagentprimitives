package probe

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// contentBlocks must surface the embedded resource's body so the MCP dispatcher
// can intercept ui:// UI resources instead of flattening them to a bare URI.
func TestContentBlocks_EmbeddedResourceCarriesBody(t *testing.T) {
	blocks := contentBlocks([]mcp.Content{
		&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{
			URI:      "ui://demo/widget",
			MIMEType: "text/html;profile=mcp-app",
			Text:     "<div>demo</div>",
			Meta:     mcp.Meta{"ui": map[string]any{"csp": map[string]any{"connectDomains": []any{"example.test"}}}},
		}},
	})
	require.Len(t, blocks, 1)
	b := blocks[0]
	assert.Equal(t, "resource", b.Type)
	assert.Equal(t, "ui://demo/widget", b.URI)
	assert.Equal(t, "text/html;profile=mcp-app", b.ResourceMIMEType)
	assert.Equal(t, "<div>demo</div>", b.ResourceText)
	assert.NotEmpty(t, b.ResourceMeta, "resource _meta must be captured for later CSP parsing")
}

// A resource with only a blob body still surfaces the bytes.
func TestContentBlocks_EmbeddedResourceBlob(t *testing.T) {
	blocks := contentBlocks([]mcp.Content{
		&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{
			URI:  "ui://demo/blob",
			Blob: []byte("<p>blob</p>"),
		}},
	})
	require.Len(t, blocks, 1)
	assert.Equal(t, []byte("<p>blob</p>"), blocks[0].ResourceBlob)
}

// toolFromSDK must recover MCP Apps' _meta.ui.visibility the same way it
// already recovers SEP-1913's top-level trust annotations from _meta —
// visibility is nested one level deeper, under "ui", as a sibling of the
// "ui.csp" fixture used above.
func TestToolFromSDK_Visibility(t *testing.T) {
	cases := []struct {
		name         string
		tool         *mcp.Tool
		wantVis      []string
		wantReadOnly bool
	}{
		{
			name: "ui.visibility=[app] -> Visibility=[app]",
			tool: &mcp.Tool{
				Name: "widget_tool",
				Meta: mcp.Meta{"ui": map[string]any{"visibility": []any{"app"}}},
			},
			wantVis: []string{"app"},
		},
		{
			name: "ui.visibility=[model] -> Visibility=[model]",
			tool: &mcp.Tool{
				Name: "model_tool",
				Meta: mcp.Meta{"ui": map[string]any{"visibility": []any{"model"}}},
			},
			wantVis: []string{"model"},
		},
		{
			name: "absent ui -> Visibility nil (synthesize.go treats this as model-visible only)",
			tool: &mcp.Tool{
				Name: "plain_tool",
			},
			wantVis: nil,
		},
		{
			name: "ui.visibility present alongside readOnlyHint -> both captured",
			tool: &mcp.Tool{
				Name:        "readonly_widget_tool",
				Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
				Meta:        mcp.Meta{"ui": map[string]any{"visibility": []any{"app"}}},
			},
			wantVis:      []string{"app"},
			wantReadOnly: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toolFromSDK(tc.tool)
			assert.Equal(t, tc.wantVis, got.Annotations.Visibility)
			assert.Equal(t, tc.wantReadOnly, got.Annotations.ReadOnlyHint)
		})
	}
}
