package meta

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uiselect"
)

// updateViewNodeDescriptionExample pulls the selector example the AGENT reads
// out of updateViewNodeDescription itself, rather than retyping it, so this
// file cannot drift into pinning a second, independently-spelled copy of the
// same literal.
func updateViewNodeDescriptionExample() string {
	const marker = `e.g. "`
	i := indexOf(updateViewNodeDescription, marker)
	if i < 0 {
		return ""
	}
	start := i + len(marker)
	end := indexOf(updateViewNodeDescription[start:], `"`)
	if end < 0 {
		return ""
	}
	return updateViewNodeDescription[start : start+end]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestTheDocumentedSelectorExampleParses guards the description's own worked
// example: if the string an agent is told to copy is not a legal
// pkg/web/uiselect selector, every agent that follows the description writes a
// binding the syntax gate then rejects. The example is read OUT of the
// constant (see updateViewNodeDescriptionExample), not retyped here, so a
// change to the documented example is what this test actually pins.
func TestTheDocumentedSelectorExampleParses(t *testing.T) {
	example := updateViewNodeDescriptionExample()
	require.NotEmpty(t, example, "updateViewNodeDescription must carry a worked selector example introduced by `e.g. \"...\"`")

	_, err := uiselect.Parse(example)
	assert.NoError(t, err, "the selector example the agent is told to copy must itself be legal uiselect syntax")
}

// TestUpdateViewSchemaCarriesTheNodeDescription pins updateViewNodeDescription
// to the wire: buildUpdateViewSchema must publish exactly this constant as
// properties.node.description, so extracting the constant cannot silently
// leave the old inline string in place at the one call site that matters.
func TestUpdateViewSchemaCarriesTheNodeDescription(t *testing.T) {
	raw := buildUpdateViewSchema([]uicomponents.Hook{{Name: "panel", AllowedComponents: []string{uicomponents.AllowAll}}})

	var schema struct {
		Properties struct {
			Node struct {
				Description string `json:"description"`
			} `json:"node"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(raw, &schema))

	assert.Equal(t, updateViewNodeDescription, schema.Properties.Node.Description)
}

// TestAFragmentWrittenAsDocumentedIsAccepted is J5's spanning test: it drives
// a binding spelled EXACTLY as updateViewNodeDescription documents through
// the real uicomponents.ParseNode and the real uicomponents.Validate, with a
// grant that admits the named tool as readonly. Asserting only that the
// description Contains("select") would pin prose and stay green even if the
// documented spelling were something pkg/web/uiselect's parser rejects — this
// test instead proves the description and the parser agree.
func TestAFragmentWrittenAsDocumentedIsAccepted(t *testing.T) {
	nodeJSON := `{"component":"ap:table","props":{"columns":[],"rows":[]},` +
		`"bindings":{"rows":{"source":"tool","ref":"demo_list","select":"results[].properties"}}}`

	node, err := uicomponents.ParseNode([]byte(nodeJSON))
	require.NoError(t, err, "a fragment spelled exactly as the agent is told to write it must parse")

	// CompileSlots is the shim that turns a legacy slot into a hook (a
	// root ap:stack whose sole child is the "panel" hook) — the tree shape
	// Validate now requires; a bare Declaration{Slots: …} is rejected as
	// unnormalized.
	decl := uicomponents.Declaration{
		View: uicomponents.CompileSlots([]uicomponents.Slot{{Name: "panel", AgentWritable: true, Default: &node}}),
	}
	opts := uicomponents.DefaultOptions()
	opts.GrantedTools = map[string]bool{"demo_list": true}
	opts.ReadonlyTools = map[string]bool{"demo_list": true}

	assert.NoError(t, uicomponents.Validate(decl, opts),
		"a binding spelled exactly as updateViewNodeDescription documents, against a tool granted as readonly, must validate")
}
