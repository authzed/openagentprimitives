package slack

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

func TestKindCapabilitiesIncludesComponents(t *testing.T) {
	caps := (&Kind{}).Capabilities()
	assert.Contains(t, caps, "components", "slack advertises the components capability so respond_to_user exposes the blocks field")
}

func TestComponentFormattingInstructionsCoverTheContract(t *testing.T) {
	got := (&Kind{}).ComponentFormattingInstructions()
	require.NotEmpty(t, got, "a components-capable kind must supply authoring instructions")
	lower := strings.ToLower(got)
	// Names the supported layout blocks.
	for _, want := range []string{"header", "section", "divider", "context", "rich_text"} {
		assert.Contains(t, lower, want, "instructions should name the %q block", want)
	}
	// States the key prohibitions and the text-fallback requirement.
	assert.Contains(t, lower, "interactive", "instructions should say interactive elements are unsupported")
	assert.Contains(t, lower, "image", "instructions should say images are not yet supported")
	assert.Contains(t, lower, "text", "instructions should tell the model to also supply a text summary/fallback")
}

// The slack Kind must satisfy the neutral component seam used by respond_to_user.
func TestKindSatisfiesComponentValidator(t *testing.T) {
	var _ channelkinds.ComponentValidator = (*Kind)(nil)
	var _ channelkinds.ComponentFormatter = (*Kind)(nil)

	k := &Kind{}
	require.NoError(t, k.ValidateComponents(json.RawMessage(`[{"type":"divider"}]`)))
	require.Error(t, k.ValidateComponents(json.RawMessage(`[{"type":"actions","elements":[]}]`)))
}

func TestComponentsPlainText(t *testing.T) {
	k := &Kind{}
	got := k.ComponentsPlainText(json.RawMessage(`[{"type":"header","text":{"type":"plain_text","text":"Results"}},{"type":"section","text":{"type":"mrkdwn","text":"all good"}}]`))
	assert.Contains(t, got, "Results")
	assert.Contains(t, got, "all good")
	assert.Equal(t, "", k.ComponentsPlainText(json.RawMessage(`not json`)), "unparseable input yields empty projection")
}

// Everything that renders reaches the egress gate's projection: a link's URL
// (not only its label), a task card source's URL, and a table keeps its rows.
func TestComponentsPlainText_ProjectsURLsAndTableRows(t *testing.T) {
	k := &Kind{}
	cases := []struct {
		name, json string
		want       []string
	}{
		{
			name: "rich_text link with a label: URL projected",
			json: `[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"link","text":"docs","url":"https://x.example/?k=SECRET"}]}]}]`,
			want: []string{"<https://x.example/?k=SECRET|docs>"},
		},
		{
			name: "task_card source: URL projected",
			json: `[{"type":"task_card","task_id":"t1","title":"Check","sources":[{"type":"url","url":"https://x.example/?k=SECRET","text":"ref"}]}]`,
			want: []string{"<https://x.example/?k=SECRET|ref>"},
		},
		{
			name: "table: one line per row, cells separated",
			json: `[{"type":"table","rows":[[{"type":"raw_text","text":"Name"},{"type":"raw_text","text":"Count"}],[{"type":"raw_text","text":"a"},{"type":"raw_text","text":"1"}]]}]`,
			want: []string{"Name | Count\na | 1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := k.ComponentsPlainText(json.RawMessage(tc.json))
			for _, w := range tc.want {
				assert.Contains(t, got, w)
			}
		})
	}
}
