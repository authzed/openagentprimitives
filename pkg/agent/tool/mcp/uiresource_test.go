package mcp_test

import (
	"context"
	"testing"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	mcptest "github.com/authzed/openagentprimitives/pkg/tools/mcp/testing"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A ui:// resource is intercepted: the widget goes to the side-band, and the
// model sees a trusted summary — never the raw URI or HTML.
func TestExecute_UIResourceIntercept(t *testing.T) {
	widget := &sdkmcp.CallToolResult{Content: []sdkmcp.Content{
		&sdkmcp.EmbeddedResource{Resource: &sdkmcp.ResourceContents{
			URI:      "ui://demo/widget",
			MIMEType: "text/html;profile=mcp-app",
			Text:     "<div>demo</div>",
		}},
	}}
	srv := mcptest.NewCallServer(mcptest.CallServerOpts{
		Tools: map[string]mcptest.ToolHandler{"search_issues": mcptest.StaticTool(widget)},
	})
	t.Cleanup(srv.Close)

	tools := mustSynthesize(t, srv.URL)
	_, opID, sess := newOpAndSess(t)
	res, err := tools[0].Execute(context.Background(), execEnvelope(t, opID, nil), sess)
	require.NoError(t, err, "Execute")

	require.NotNil(t, res.UIResource, "ui:// resource must be intercepted into the side-band")
	assert.Equal(t, "ui://demo/widget", res.UIResource.URI)
	assert.Equal(t, "text/html;profile=mcp-app", res.UIResource.MIMEType)
	assert.Equal(t, "<div>demo</div>", string(res.UIResource.HTML))
	assert.Equal(t, "search_issues", res.UIResource.Tool)
	assert.NotContains(t, res.Content, "ui://demo/widget", "raw widget URI must not reach the model")
	assert.NotContains(t, res.Content, "<div>demo</div>", "raw widget HTML must not reach the model")
	assert.Contains(t, res.Content, "interactive widget", "the model sees a summary, not the placeholder")
}

// A blob-bodied ui:// resource surfaces its bytes as HTML.
func TestExecute_UIResourceBlob(t *testing.T) {
	widget := &sdkmcp.CallToolResult{Content: []sdkmcp.Content{
		&sdkmcp.EmbeddedResource{Resource: &sdkmcp.ResourceContents{
			URI:  "ui-app://demo/blob",
			Blob: []byte("<p>blob</p>"),
		}},
	}}
	srv := mcptest.NewCallServer(mcptest.CallServerOpts{
		Tools: map[string]mcptest.ToolHandler{"search_issues": mcptest.StaticTool(widget)},
	})
	t.Cleanup(srv.Close)

	tools := mustSynthesize(t, srv.URL)
	_, opID, sess := newOpAndSess(t)
	res, err := tools[0].Execute(context.Background(), execEnvelope(t, opID, nil), sess)
	require.NoError(t, err, "Execute")

	require.NotNil(t, res.UIResource)
	assert.Equal(t, []byte("<p>blob</p>"), res.UIResource.HTML)
}

// A non-ui:// resource is NOT intercepted — it still flattens to the placeholder.
func TestExecute_NonUIResource_StillFlattens(t *testing.T) {
	doc := &sdkmcp.CallToolResult{Content: []sdkmcp.Content{
		&sdkmcp.EmbeddedResource{Resource: &sdkmcp.ResourceContents{
			URI:      "file:///report.pdf",
			MIMEType: "application/pdf",
		}},
	}}
	srv := mcptest.NewCallServer(mcptest.CallServerOpts{
		Tools: map[string]mcptest.ToolHandler{"search_issues": mcptest.StaticTool(doc)},
	})
	t.Cleanup(srv.Close)

	tools := mustSynthesize(t, srv.URL)
	_, opID, sess := newOpAndSess(t)
	res, err := tools[0].Execute(context.Background(), execEnvelope(t, opID, nil), sess)
	require.NoError(t, err, "Execute")

	assert.Nil(t, res.UIResource, "a non-ui:// resource must NOT be intercepted")
	assert.Contains(t, res.Content, "[resource: file:///report.pdf]", "non-ui resources still flatten to the placeholder")
}

// An error result carrying a ui:// resource must NOT attach the widget, and the
// summary must not falsely claim it was shown.
func TestExecute_UIResource_ErrorResult_NotAttached(t *testing.T) {
	widget := &sdkmcp.CallToolResult{IsError: true, Content: []sdkmcp.Content{
		&sdkmcp.EmbeddedResource{Resource: &sdkmcp.ResourceContents{
			URI:      "ui://demo/widget",
			MIMEType: "text/html;profile=mcp-app",
			Text:     "<div>demo</div>",
		}},
	}}
	srv := mcptest.NewCallServer(mcptest.CallServerOpts{
		Tools: map[string]mcptest.ToolHandler{"search_issues": mcptest.StaticTool(widget)},
	})
	t.Cleanup(srv.Close)

	tools := mustSynthesize(t, srv.URL)
	_, opID, sess := newOpAndSess(t)
	res, err := tools[0].Execute(context.Background(), execEnvelope(t, opID, nil), sess)
	require.NoError(t, err, "Execute")

	assert.True(t, res.IsError, "error result stays IsError")
	assert.Nil(t, res.UIResource, "widget must NOT be attached on an error result")
	assert.NotContains(t, res.Content, "shown to the user", "must not falsely claim the widget was shown")
}

var _ = agenttool.UIResourceSpec{} // compile guard: the side-band type exists
