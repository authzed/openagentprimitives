package slack

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// vizLineChart builds a line-chart data_visualization block with `points`
// x-axis categories and one series carrying exactly one aligned point each —
// the shape a time-series answer produces. Used to exercise Slack's 1–20
// category/point cap.
func vizLineChart(points int) string {
	cats := make([]string, points)
	data := make([]string, points)
	for i := range cats {
		cats[i] = fmt.Sprintf(`"t%d"`, i)
		data[i] = fmt.Sprintf(`{"label":"t%d","value":%d}`, i, i)
	}
	return `[{"type":"data_visualization","title":"p95","chart":{"type":"line",` +
		`"series":[{"name":"p95","data":[` + strings.Join(data, ",") + `]}],` +
		`"axis_config":{"categories":[` + strings.Join(cats, ",") + `]}}}]`
}

// vizManySeries builds a bar chart with `n` single-point series over one
// category, to exercise Slack's 1–6 series-per-chart cap.
func vizManySeries(n int) string {
	series := make([]string, n)
	for i := range series {
		series[i] = fmt.Sprintf(`{"name":"s%d","data":[{"label":"Mon","value":%d}]}`, i, i)
	}
	return `[{"type":"data_visualization","title":"x","chart":{"type":"bar",` +
		`"series":[` + strings.Join(series, ",") + `],` +
		`"axis_config":{"categories":["Mon"]}}}]`
}

// vizBar builds a bar chart from a raw categories array and raw series
// objects, so each case states only the part of the chart it is breaking.
func vizBar(categories, series string) string {
	return `[{"type":"data_visualization","title":"x","chart":{"type":"bar",` +
		`"series":[` + series + `],"axis_config":{"categories":` + categories + `}}}]`
}

// vizPie builds a pie chart from raw segment objects.
func vizPie(segments string) string {
	return `[{"type":"data_visualization","title":"x","chart":{"type":"pie","segments":[` + segments + `]}}]`
}

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

func headerWith(char string, n int) string {
	return `[{"type":"header","text":{"type":"plain_text","text":"` + strings.Repeat(char, n) + `"}}]`
}

func sectionWith(char string, n int) string {
	return `[{"type":"section","text":{"type":"mrkdwn","text":"` + strings.Repeat(char, n) + `"}}]`
}

const pieChart = `{"type":"data_visualization","title":"Share","chart":{"type":"pie","segments":[{"label":"A","value":1}]}}`

func rawTextTable(rows, cols int) string {
	cells := make([]string, cols)
	for i := range cells {
		cells[i] = `{"type":"raw_text","text":"x"}`
	}
	row := "[" + strings.Join(cells, ",") + "]"
	rs := make([]string, rows)
	for i := range rs {
		rs[i] = row
	}
	return `{"type":"table","rows":[` + strings.Join(rs, ",") + `]}`
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
		{name: "section <!subteam^>: rejected usergroup ping", json: `[{"type":"section","text":{"type":"mrkdwn","text":"hey <!subteam^S12345> look"}}]`, wantErr: "usergroup"},
		{name: "rich_text usergroup element: rejected usergroup ping", json: `[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"usergroup","usergroup_id":"S12345"}]}]}]`, wantErr: "usergroup"},
		{name: "data_viz pie segment label <!channel>: rejected broadcast", json: `[{"type":"data_visualization","title":"Share","chart":{"type":"pie","segments":[{"label":"<!channel>","value":60}]}}]`, wantErr: "broadcast"},
		{name: "data_viz bar series name <!subteam^>: rejected usergroup ping", json: `[{"type":"data_visualization","title":"Signups","chart":{"type":"bar","series":[{"name":"<!subteam^S1>","data":[{"label":"Mon","value":10}]}],"axis_config":{"categories":["Mon"]}}}]`, wantErr: "usergroup"},
		{name: "data_viz axis category <!here>: rejected broadcast", json: `[{"type":"data_visualization","title":"Signups","chart":{"type":"line","series":[{"name":"S","data":[{"label":"<!here>","value":10}]}],"axis_config":{"categories":["<!here>"]}}}]`, wantErr: "broadcast"},

		// --- one block slot is reserved for the sender's mention_only footer ---
		{name: "49 blocks: accepted at the agent cap", json: repeatBlock(`{"type":"divider"}`, 49), wantErr: ""},
		{name: "50 blocks: rejected (one slot reserved for the footer)", json: repeatBlock(`{"type":"divider"}`, 50), wantErr: "too many blocks"},

		// --- limits count characters, not bytes ---
		{name: "header of 150 CJK chars (450 bytes): accepted", json: headerWith("日", 150), wantErr: ""},
		{name: "header of 151 CJK chars: rejected, counted in characters", json: headerWith("日", 151), wantErr: "text is 151 characters"},
		{name: "section of 3000 CJK chars (9000 bytes): accepted", json: sectionWith("日", 3000), wantErr: ""},
		{name: "markdown of 12000 CJK chars: accepted", json: `[{"type":"markdown","text":"` + strings.Repeat("日", 12000) + `"}]`, wantErr: ""},

		// --- per-message chart and table counts, containers included ---
		{name: "2 charts: accepted", json: repeatBlock(pieChart, 2), wantErr: ""},
		{name: "3 charts: rejected", json: repeatBlock(pieChart, 3), wantErr: "too many data_visualization"},
		{name: "third chart inside a container: rejected", json: `[` + pieChart + `,` + pieChart + `,{"type":"container","title":{"type":"plain_text","text":"G"},"child_blocks":[` + pieChart + `]}]`, wantErr: "too many data_visualization"},
		{name: "2 tables: rejected", json: repeatBlock(rawTextTable(1, 1), 2), wantErr: "too many table"},
		{name: "table of 100 rows x 20 columns: accepted", json: `[` + rawTextTable(100, 20) + `]`, wantErr: ""},
		{name: "table of 101 rows: rejected", json: `[` + rawTextTable(101, 1) + `]`, wantErr: "101 rows"},
		{name: "table row of 21 cells: rejected", json: `[` + rawTextTable(1, 21) + `]`, wantErr: "21 cells"},

		// --- data_visualization per-chart limits (Slack caps; slack-go
		// block_data_visualization.go documents 1–6 series/segments, 1–20
		// categories with exactly one point per category, ≤20-char labels) ---
		{name: "line chart with 20 points: accepted at the cap", json: vizLineChart(20), wantErr: ""},
		{name: "line chart with 21 points: rejected over cap", json: vizLineChart(21), wantErr: "cannot have more than 20 points"},
		{name: "bar chart with 6 series: accepted at the cap", json: vizManySeries(6), wantErr: ""},
		{name: "bar chart with 7 series: rejected over cap", json: vizManySeries(7), wantErr: "cannot have more than 6 series"},
		{name: "series point count != categories: rejected", json: vizBar(`["Mon"]`, `{"name":"S","data":[{"label":"Mon","value":1},{"label":"Tue","value":2}]}`), wantErr: "exactly one point for every category"},
		{name: "point label not matching its category: rejected", json: vizBar(`["2026-10-01","2026-10-02"]`, `{"name":"S","data":[{"label":"Oct 1","value":1},{"label":"Oct 2","value":2}]}`), wantErr: "must match axis_config.categories"},
		{name: "repeated point label within a series: rejected", json: vizBar(`["Mon","Tue"]`, `{"name":"S","data":[{"label":"Mon","value":1},{"label":"Mon","value":2}]}`), wantErr: "duplicate label"},
		{name: "repeated category: rejected", json: vizBar(`["Mon","Mon"]`, `{"name":"S","data":[{"label":"Mon","value":1},{"label":"Mon","value":2}]}`), wantErr: "duplicate category"},
		{name: "empty category: rejected", json: vizBar(`[""]`, `{"name":"S","data":[{"label":"","value":1}]}`), wantErr: "categories[0] must have a minimum length of 1"},
		{name: "category label over 20 chars: rejected", json: vizBar(`["`+strings.Repeat("c", 21)+`"]`, `{"name":"S","data":[{"label":"`+strings.Repeat("c", 21)+`","value":1}]}`), wantErr: "categories[0] cannot be longer than 20"},
		{name: "empty series name: rejected", json: vizBar(`["Mon"]`, `{"name":"","data":[{"label":"Mon","value":1}]}`), wantErr: "name must have a minimum length of 1"},
		{name: "series name over 20 chars: rejected", json: vizBar(`["Mon"]`, `{"name":"`+strings.Repeat("s", 21)+`","data":[{"label":"Mon","value":1}]}`), wantErr: "name cannot be longer than 20"},
		{name: "empty data point label: rejected", json: vizBar(`["Mon"]`, `{"name":"S","data":[{"label":"","value":1}]}`), wantErr: "label must have a minimum length of 1"},
		{name: "duplicate series names: rejected", json: vizBar(`["Mon"]`, `{"name":"S","data":[{"label":"Mon","value":1}]},{"name":"S","data":[{"label":"Mon","value":2}]}`), wantErr: "series names must be unique"},
		{name: "pie chart with 7 segments: rejected over cap", json: `[{"type":"data_visualization","title":"x","chart":{"type":"pie","segments":[{"label":"a","value":1},{"label":"b","value":1},{"label":"c","value":1},{"label":"d","value":1},{"label":"e","value":1},{"label":"f","value":1},{"label":"g","value":1}]}}]`, wantErr: "more than 6 segments"},
		{name: "pie segment label over 20 chars: rejected", json: vizPie(`{"label":"` + strings.Repeat("a", 21) + `","value":1}`), wantErr: "label cannot be longer than 20"},
		{name: "pie segment with empty label: rejected", json: vizPie(`{"label":"","value":1}`), wantErr: "label must have a minimum length of 1"},
		{name: "pie segment with zero value: rejected", json: vizPie(`{"label":"Sales","value":5},{"label":"Refunds","value":0}`), wantErr: "value must be greater than 0"},
		{name: "pie segment with negative value: rejected", json: vizPie(`{"label":"Refunds","value":-1}`), wantErr: "value must be greater than 0"},
		{name: "chart rejection: carries the full rule set for the model", json: vizLineChart(21), wantErr: "bucket long time ranges"},

		// --- structurally empty blocks Slack rejects ---
		{name: "header without text: rejected", json: `[{"type":"header"}]`, wantErr: "header"},
		{name: "header with blank text: rejected", json: `[{"type":"header","text":{"type":"plain_text","text":"  "}}]`, wantErr: "non-empty"},
		{name: "section with neither text nor fields: rejected", json: `[{"type":"section"}]`, wantErr: "fields"},
		{name: "context with no elements: rejected", json: `[{"type":"context","elements":[]}]`, wantErr: "at least one"},
		{name: "markdown with empty text: rejected", json: `[{"type":"markdown","text":""}]`, wantErr: "non-empty"},
		{name: "table with no rows: rejected", json: `[{"type":"table","rows":[]}]`, wantErr: "at least one row"},
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
