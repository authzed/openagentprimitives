package slack

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func repeatBlock(block string, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = block
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func sectionWithText(n int) string {
	return `[{"type":"section","text":{"type":"mrkdwn","text":"` + strings.Repeat("a", n) + `"}}]`
}

func headerWithText(n int) string {
	return `[{"type":"header","text":{"type":"plain_text","text":"` + strings.Repeat("a", n) + `"}}]`
}

func sectionWithFields(n int) string {
	fs := make([]string, n)
	for i := range fs {
		fs[i] = `{"type":"mrkdwn","text":"f"}`
	}
	return `[{"type":"section","fields":[` + strings.Join(fs, ",") + `]}]`
}

func contextWithTextElements(n int) string {
	es := make([]string, n)
	for i := range es {
		es[i] = `{"type":"mrkdwn","text":"e"}`
	}
	return `[{"type":"context","elements":[` + strings.Join(es, ",") + `]}]`
}

func TestValidateComponents(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		wantErr string // "" means expect nil; otherwise err.Error() must contain this
	}{
		// --- accepted presentational layouts ---
		{
			name:    "header+section+divider+context: accepted",
			json:    `[{"type":"header","text":{"type":"plain_text","text":"Results"}},{"type":"section","text":{"type":"mrkdwn","text":"*All good.*"}},{"type":"divider"},{"type":"context","elements":[{"type":"mrkdwn","text":"as of 10:00"}]}]`,
			wantErr: "",
		},
		{
			name:    "section with fields: accepted",
			json:    `[{"type":"section","fields":[{"type":"mrkdwn","text":"*Status*\nOK"},{"type":"mrkdwn","text":"*Count*\n42"}]}]`,
			wantErr: "",
		},
		{
			name:    "rich_text section: accepted",
			json:    `[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"hello"}]}]}]`,
			wantErr: "",
		},
		{
			name:    "markdown block: accepted",
			json:    `[{"type":"markdown","text":"## Summary\n- one\n- two"}]`,
			wantErr: "",
		},
		{
			name:    "table with rich_text + raw_text cells: accepted",
			json:    `[{"type":"table","rows":[[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"Name"}]}]},{"type":"raw_text","text":"OK"}]]}]`,
			wantErr: "",
		},
		{
			name:    "data_visualization pie chart: accepted",
			json:    `[{"type":"data_visualization","title":"Share","chart":{"type":"pie","segments":[{"label":"A","value":60},{"label":"B","value":40}]}}]`,
			wantErr: "",
		},
		{
			name:    "card text-only: accepted",
			json:    `[{"type":"card","title":{"type":"plain_text","text":"Release 1.2"},"body":{"type":"mrkdwn","text":"Shipped *today*."}}]`,
			wantErr: "",
		},
		{
			name:    "container with title + read-only children: accepted",
			json:    `[{"type":"container","title":{"type":"plain_text","text":"Group"},"child_blocks":[{"type":"header","text":{"type":"plain_text","text":"Sub"}},{"type":"section","text":{"type":"mrkdwn","text":"inside"}}]}]`,
			wantErr: "",
		},
		{
			name:    "bar chart with series+axis_config: accepted",
			json:    `[{"type":"data_visualization","title":"Signups","chart":{"type":"bar","series":[{"name":"S","data":[{"label":"Mon","value":10}]}],"axis_config":{"categories":["Mon"]}}}]`,
			wantErr: "",
		},

		// --- shape errors ---
		{name: "empty array: rejected", json: `[]`, wantErr: "at least one block"},
		{name: "not an array: rejected", json: `{"type":"section"}`, wantErr: "must be a JSON array"},
		{name: "malformed json: rejected", json: `[{"type":`, wantErr: "could not be parsed"},

		// --- interactivity rejected (presentation-only scope) ---
		{name: "actions block: rejected", json: `[{"type":"actions","elements":[{"type":"button","text":{"type":"plain_text","text":"Go"},"action_id":"a"}]}]`, wantErr: "interactive"},
		{name: "input block: rejected", json: `[{"type":"input","label":{"type":"plain_text","text":"Name"},"element":{"type":"plain_text_input","action_id":"a"}}]`, wantErr: "interactive"},
		{name: "section with button accessory: rejected", json: `[{"type":"section","text":{"type":"mrkdwn","text":"hi"},"accessory":{"type":"button","text":{"type":"plain_text","text":"Go"},"action_id":"a"}}]`, wantErr: "accessor"},
		{name: "context_actions block: rejected", json: `[{"type":"context_actions","elements":[{"type":"button","text":{"type":"plain_text","text":"x"},"action_id":"a"}]}]`, wantErr: "interactive"},
		{name: "data_table block: rejected as interactive", json: `[{"type":"data_table","column_settings":[],"rows":[]}]`, wantErr: "table"},
		{name: "carousel block: rejected as interactive", json: `[{"type":"carousel","elements":[]}]`, wantErr: "carousel"},
		{name: "card with action buttons: rejected", json: `[{"type":"card","title":{"type":"plain_text","text":"x"},"actions":{"elements":[{"type":"button","text":{"type":"plain_text","text":"Go"},"action_id":"a"}]}}]`, wantErr: "action"},
		{name: "container with interactive child: rejected", json: `[{"type":"container","title":{"type":"plain_text","text":"G"},"child_blocks":[{"type":"actions","elements":[{"type":"button","text":{"type":"plain_text","text":"x"},"action_id":"a"}]}]}]`, wantErr: "interactive"},
		{name: "container without title: rejected", json: `[{"type":"container","child_blocks":[{"type":"section","text":{"type":"mrkdwn","text":"x"}}]}]`, wantErr: "title"},
		{name: "data_visualization without title: rejected", json: `[{"type":"data_visualization","chart":{"type":"pie","segments":[{"label":"A","value":1}]}}]`, wantErr: "title"},
		{name: "bar chart missing axis_config: rejected", json: `[{"type":"data_visualization","title":"x","chart":{"type":"bar","series":[{"name":"S","data":[{"label":"Mon","value":1}]}]}}]`, wantErr: "axis_config"},
		{name: "pie chart with empty segments: rejected", json: `[{"type":"data_visualization","title":"x","chart":{"type":"pie","segments":[]}}]`, wantErr: "segments"},

		// --- alert is modals-only, not valid in a message ---
		{name: "alert block: rejected (modals-only)", json: `[{"type":"alert","level":"warning","text":{"type":"mrkdwn","text":"heads up"}}]`, wantErr: "modals"},

		// --- images deferred to slice 2 ---
		{name: "image block: rejected as not-yet-supported", json: `[{"type":"image","image_url":"https://example.com/x.png","alt_text":"x"}]`, wantErr: "image"},
		{name: "context image element: rejected as not-yet-supported", json: `[{"type":"context","elements":[{"type":"image","image_url":"https://example.com/x.png","alt_text":"x"}]}]`, wantErr: "image"},
		{name: "card with hero image: rejected as not-yet-supported", json: `[{"type":"card","title":{"type":"plain_text","text":"x"},"hero_image":{"type":"image","image_url":"https://example.com/x.png","alt_text":"x"}}]`, wantErr: "image"},
		{name: "table raw_number cell: rejected for messages", json: `[{"type":"table","rows":[[{"type":"raw_number","value":42}]]}]`, wantErr: "raw_number"},

		// --- unsupported block types ---
		{name: "unknown block type: rejected", json: `[{"type":"made_up_block"}]`, wantErr: "unsupported block type"},
		{name: "video block: rejected as unsupported", json: `[{"type":"video","title":{"type":"plain_text","text":"x"},"video_url":"https://x","thumbnail_url":"https://x","alt_text":"x"}]`, wantErr: "unsupported block type"},

		// --- structural limits ---
		{name: "51 blocks: rejected over limit", json: repeatBlock(`{"type":"divider"}`, 51), wantErr: "too many blocks"},
		{name: "section text over 3000: rejected", json: sectionWithText(3001), wantErr: "3000"},
		{name: "header over 150: rejected", json: headerWithText(151), wantErr: "150"},
		{name: "11 section fields: rejected over limit", json: sectionWithFields(11), wantErr: "fields"},
		{name: "11 context elements: rejected over limit", json: contextWithTextElements(11), wantErr: "context"},

		// --- broadcast mentions rejected ---
		{name: "section <!channel>: rejected broadcast", json: `[{"type":"section","text":{"type":"mrkdwn","text":"hey <!channel> look"}}]`, wantErr: "broadcast"},
		{name: "context <!here>: rejected broadcast", json: `[{"type":"context","elements":[{"type":"mrkdwn","text":"<!here> ping"}]}]`, wantErr: "broadcast"},
		{name: "section field <!everyone>: rejected broadcast", json: `[{"type":"section","fields":[{"type":"mrkdwn","text":"<!everyone>"}]}]`, wantErr: "broadcast"},
		{name: "rich_text broadcast element: rejected", json: `[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"broadcast","range":"channel"}]}]}]`, wantErr: "broadcast"},
		{name: "markdown <!channel>: rejected broadcast", json: `[{"type":"markdown","text":"ping <!channel> now"}]`, wantErr: "broadcast"},
		{name: "table raw_text cell <!here>: rejected broadcast", json: `[{"type":"table","rows":[[{"type":"raw_text","text":"<!here> look"}]]}]`, wantErr: "broadcast"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateComponents([]byte(tc.json))
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
