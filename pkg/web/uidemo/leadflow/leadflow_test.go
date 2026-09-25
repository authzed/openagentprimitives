package leadflow

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decodeResult unmarshals a tool result's single TextContent (the JSON
// document every leadflow tool returns) into out — the same shape
// pkg/web/uibindings/tool.unwrapResult expects to unwrap on the real binding
// path, verified directly here without going through MCP transport.
func decodeResult(res *mcp.CallToolResult, out any) error {
	if len(res.Content) != 1 {
		return fmt.Errorf("want exactly one content block, got %d", len(res.Content))
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		return fmt.Errorf("content[0] is not TextContent: %T", res.Content[0])
	}
	return json.Unmarshal([]byte(tc.Text), out)
}

// TestAdvanceStageMovesOneStageAndRefusesTheLast is the package's own
// behavior, with no MCP transport involved: AdvanceStage must move exactly
// one stage forward, refuse to move a lead already at the last stage, and
// refuse an unknown id — never a silent no-op — leaving the book unchanged
// on every error.
func TestAdvanceStageMovesOneStageAndRefusesTheLast(t *testing.T) {
	cases := []struct {
		name    string
		id      string
		wantErr bool
		want    string
	}{
		{name: "new advances to qualified", id: "lead-aurorabyte", want: "qualified"},
		{name: "proposal advances to won", id: "lead-granitepeak", want: "won"},
		{name: "won refuses: there is no stage after the last", id: "lead-juniper", wantErr: true},
		{name: "unknown id is an error, never a silent no-op", id: "lead-absent", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := NewBook()
			before := b.Filter("", "", "")

			got, err := b.AdvanceStage(tc.id)

			after := b.Filter("", "", "")
			if tc.wantErr {
				// assert, not require: both facts must be checked independently
				// so a mutation that breaks only one of them is visible as only
				// one failing, never masked by an early abort on the other.
				assert.Error(t, err, "AdvanceStage must return an error, never a silent no-op")
				assert.Equal(t, before, after, "an error must leave the book unchanged")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.Stage)
			assert.NotEqual(t, before, after, "a successful advance must be visible in the book")
		})
	}
}

// TestStageBreakdownAlwaysReturnsEveryStage pins the chart's x-axis
// stability: a filter that empties every stage must still emit each Stages
// entry with count 0, in Stages order, rather than omitting it.
func TestStageBreakdownAlwaysReturnsEveryStage(t *testing.T) {
	b := NewBook()
	// A window that excludes every seeded lead (seed.go's dates are all in
	// 2026): still one entry per Stages element, all zero.
	got, err := stageBreakdown(b, "2099-01-01T00:00:00Z", "2099-12-31T00:00:00Z")
	require.NoError(t, err)
	require.Len(t, got, len(Stages), "one entry per declared stage, always")
	for i, want := range Stages {
		assert.Equal(t, want, got[i].Stage, "stage order must match Stages")
		assert.Zero(t, got[i].Count, "an emptied stage must report 0, not be omitted")
	}
}

// TestEveryDeclaredArgumentIsStringTyped walks the tool table's InputSchema:
// every declared property must be "type": "string", because a data binding
// can only ever substitute a JSON string (uibindings.SubstituteParams) and
// there is no InputSchema validation on the app-tool path to catch a number
// arriving as a string. It also confirms MaxRows bounds list_leads
// server-side rather than through a tool argument.
func TestEveryDeclaredArgumentIsStringTyped(t *testing.T) {
	require.NotEmpty(t, toolTable)
	for _, td := range toolTable {
		t.Run(td.Name, func(t *testing.T) {
			props, _ := td.InputSchema["properties"].(map[string]any)
			for field, raw := range props {
				schema, ok := raw.(map[string]any)
				require.Truef(t, ok, "%s: property %q schema must be a map", td.Name, field)
				assert.Equalf(t, "string", schema["type"],
					"%s: property %q must be declared type:string", td.Name, field)
			}
		})
	}

	b := NewBook()
	require.Greater(t, len(b.Filter("", "", "")), 3, "fixture must seed more leads than the row cap under test")
	s := New(Options{Book: b, MaxRows: 3})
	res, err := handleListLeads(s, nil)
	require.NoError(t, err)
	require.False(t, res.IsError, "an unfiltered list_leads call must succeed")
	var got []Lead
	require.NoError(t, decodeResult(res, &got))
	assert.Len(t, got, 3, "MaxRows must bound the result server-side, never via a tool argument")
}

// TestMalformedArgumentTypeIsAVisibleError exercises the "params are
// strings" constraint from the caller's side: every declared argument is
// string-typed (TestEveryDeclaredArgumentIsStringTyped), but nothing on the
// app-tool call path validates that a caller actually sent a string — see
// the InputSchema note on Options.MaxRows. A JSON number arriving for the
// optional `note` field must be rejected as a visible tool-level error, not
// silently coerced to "" while the call otherwise succeeds — the concrete
// failure mode a caller upstream that ever sent a raw JSON number would
// hit. note (not the required leadId) is the vector: encoding/json
// populates every field that DID typecheck before reporting the error, so
// this isolates whether the malformed-arguments check itself is doing
// work, rather than being masked by the separate empty-leadId check.
func TestMalformedArgumentTypeIsAVisibleError(t *testing.T) {
	b := NewBook()
	s := New(Options{Book: b})
	before := b.Filter("", "", "")

	res, err := handleAdvanceLeadStage(s, json.RawMessage(`{"leadId":"lead-aurorabyte","note":42}`))

	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.IsError, "a JSON number where a string is declared must be a visible tool-level error")
	require.Len(t, res.Content, 1)
	tc, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok, "error content must be TextContent")
	assert.Contains(t, tc.Text, "malformed arguments", "the error must name the cause, not a generic failure")

	after := b.Filter("", "", "")
	assert.Equal(t, before, after, "a rejected call must not mutate the book")
}
