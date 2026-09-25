package render_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/mcp/render"
	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
	toolspec "github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
)

func sampleSpec() *mcpspec.Spec {
	return &mcpspec.Spec{
		Name:    "linear",
		Version: "1",
		Tools: []mcpspec.Tool{{
			Name:                "list_issues",
			Intent:              "list Linear issues in a window",
			DescriptionOverride: "List issues created recently.",
			Args: mcpspec.Args{
				AllowedFields:   []string{"createdAfter", "teamId"},
				SensitiveFields: []string{"apiKey"},
				Constraints: []toolspec.Constraint{
					{CEL: `args.createdAfter > now() - duration("720h")`,
						Message: "Window is capped at 30 days."},
					{CEL: `args.teamId != ""`, Message: ""},
				},
			},
		}},
	}
}

func TestDescribe_MarkdownSurfacesMessageNotCEL(t *testing.T) {
	out, err := render.Describe(sampleSpec(), "list_issues", render.FormatMarkdown)
	require.NoError(t, err)
	assert.Contains(t, out, "list_issues")
	assert.Contains(t, out, "createdAfter")
	assert.Contains(t, out, "Window is capped at 30 days.")
	assert.NotContains(t, out, "now() - duration")
	assert.NotContains(t, out, `args.teamId`)
	assert.Contains(t, out, "additional constraint applies")
}

func TestDescribe_UnknownTool(t *testing.T) {
	_, err := render.Describe(sampleSpec(), "no_such_tool", render.FormatText)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no_such_tool")
}

func TestDescribe_TextFormat(t *testing.T) {
	out, err := render.Describe(sampleSpec(), "list_issues", render.FormatText)
	require.NoError(t, err)
	assert.Contains(t, out, "list_issues")
	assert.Contains(t, out, "apiKey")
	assert.NotContains(t, out, "duration(")
}
