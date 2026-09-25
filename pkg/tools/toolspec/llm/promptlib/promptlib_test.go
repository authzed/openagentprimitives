package promptlib

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/llm"
)

func TestBuildSelectPrompt_IncludesIntentAndTools(t *testing.T) {
	sums := []llm.ToolkitSummary{
		{Name: "gh", Binary: "gh", Subcommands: []llm.SubcommandSummary{
			{Path: []string{"pr", "view"}, Description: "View a PR", Reads: []string{"network"}, NetworkDestinations: []string{"api.github.com"}},
			{Path: []string{"pr", "merge"}, Description: "Merge a PR", Destructive: true, Writes: []string{"network"}},
		}},
	}
	sys, user := BuildSelectPrompt("test intent", sums)
	assert.Contains(t, user, "test intent", "user prompt should carry intent")
	assert.Contains(t, sys, "pr view", "system prompt should list subcommand")
	assert.Contains(t, sys, "DESTRUCTIVE", "system prompt should flag destructive subcommands")
	assert.Contains(t, sys, "toolkitName", "system prompt should mention response JSON schema")
}

func TestParseSelectResponse_HappyAndEmpty(t *testing.T) {
	cases := []struct {
		name            string
		in              string
		wantToolkitName string
		wantUnmatched   int
	}{
		{
			name:            "happy: toolkit name + reasoning round-trip",
			in:              `{"toolkitName":"gh","reasoning":"matches","unmatched":[]}`,
			wantToolkitName: "gh",
			wantUnmatched:   0,
		},
		{
			name:            "empty toolkit name + populated unmatched",
			in:              `{"toolkitName":"","reasoning":"nothing matched","unmatched":[{"request":"x","reason":"y"}]}`,
			wantToolkitName: "",
			wantUnmatched:   1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := ParseSelectResponse(tc.in)
			require.NoError(t, err, "ParseSelectResponse")
			assert.Equal(t, tc.wantToolkitName, r.ToolkitName, "ToolkitName")
			assert.Len(t, r.Unmatched, tc.wantUnmatched, "Unmatched length")
		})
	}
}

func TestParseSelectResponse_BadJSON(t *testing.T) {
	_, err := ParseSelectResponse("not json")
	assert.Error(t, err, "expected parse error")
}

// TestStripFences verifies the JSON-decode helper strips markdown code fences.
func TestStripFences(t *testing.T) {
	raw := "```json\n{\"toolkitName\":\"x\"}\n```"
	cleaned := StripFences(raw)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(cleaned), &m), "cleaned should parse")
}

func TestBuildGeneratePrompt(t *testing.T) {
	cases := []struct {
		name           string
		req            llm.GenerateRequest
		userContains   []string
		userNotContain []string
		sysContains    []string
	}{
		{
			name: "no prior error: user carries intent + yaml, no error block",
			req: llm.GenerateRequest{
				Intent: "intent-x", ToolkitName: "gh", ToolkitYAML: "<yaml-goes-here>",
			},
			userContains:   []string{"intent-x", "<yaml-goes-here>"},
			userNotContain: []string{"previous attempt"},
			sysContains:    []string{"descriptions", "specYAML"},
		},
		{
			name: "with prior error: user includes error header + text + fix instruction",
			req: llm.GenerateRequest{
				Intent: "intent-x", ToolkitName: "gh", ToolkitYAML: "<yaml>",
				PriorError: "yaml unmarshal: cannot unmarshal bool into []string",
			},
			userContains: []string{
				"Your previous attempt failed validation:",
				"yaml unmarshal: cannot unmarshal bool into []string",
				"Please fix the issue and emit a valid spec.",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sys, user := BuildGeneratePrompt(tc.req)
			for _, s := range tc.userContains {
				assert.Contains(t, user, s, "user prompt should contain %q", s)
			}
			for _, s := range tc.userNotContain {
				assert.NotContains(t, user, s, "user prompt should not contain %q", s)
			}
			for _, s := range tc.sysContains {
				assert.Contains(t, sys, s, "system prompt should contain %q", s)
			}
		})
	}
}

func TestParseGenerateResponse_Full(t *testing.T) {
	raw := `{
  "specYAML": "name: s\nversion: \"1\"\n",
  "descriptions": {"allowSubcommands[0]": "look at a PR"},
  "warnings":  ["w1"],
  "excluded":  [{"name":"pr comment","reason":"writes"}],
  "unmatched": [{"request":"r","reason":"why"}]
}`
	r, err := ParseGenerateResponse(raw)
	require.NoError(t, err, "ParseGenerateResponse")
	assert.Contains(t, r.SpecYAML, "name: s", "SpecYAML")
	assert.Equal(t, "look at a PR", r.Descriptions["allowSubcommands[0]"], "Descriptions")
	assert.Len(t, r.Warnings, 1, "Warnings length")
	assert.Len(t, r.Excluded, 1, "Excluded length")
	assert.Len(t, r.Unmatched, 1, "Unmatched length")
}

func TestBuildRefinePrompt(t *testing.T) {
	baseMismatch := []llm.Mismatch{{
		TestCase: llm.TestCase{Intent: "should allow view", Argv: []string{"pr", "view"}, ExpectAllow: true},
		Actual:   false, Reason: "allowSubcommands",
	}}
	cases := []struct {
		name           string
		req            llm.RefineRequest
		userContains   []string
		userNotContain []string
		sysContains    []string
	}{
		{
			name: "mismatches + prior spec rendered in user prompt",
			req: llm.RefineRequest{
				Intent:            "intent",
				ToolkitName:       "gh",
				ToolkitYAML:       "<yaml>",
				PriorSpecYAML:     "<prior>",
				PriorDescriptions: map[string]string{"allowSubcommands[0]": "look"},
				Mismatches:        baseMismatch,
			},
			userContains: []string{"should allow view", "<prior>"},
			sysContains:  []string{"Rewrite the spec YAML end-to-end"},
		},
		{
			name: "prior error: header + text + fix instruction in user prompt",
			req: llm.RefineRequest{
				Intent:        "intent",
				ToolkitName:   "gh",
				ToolkitYAML:   "<yaml>",
				PriorSpecYAML: "<prior>",
				Mismatches:    baseMismatch,
				PriorError:    "yaml: cannot unmarshal bool into []string",
			},
			userContains: []string{
				"Your previous refinement failed validation:",
				"yaml: cannot unmarshal bool into []string",
				"Please fix the issue and emit a valid spec.",
			},
		},
		{
			name: "no prior error: user should omit error block",
			req: llm.RefineRequest{
				Intent:        "intent",
				ToolkitName:   "gh",
				ToolkitYAML:   "<yaml>",
				PriorSpecYAML: "<prior>",
				Mismatches: []llm.Mismatch{
					{TestCase: llm.TestCase{Intent: "case", ExpectAllow: true}, Actual: false},
				},
			},
			userNotContain: []string{"previous refinement failed"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sys, user := BuildRefinePrompt(tc.req)
			for _, s := range tc.userContains {
				assert.Contains(t, user, s, "user prompt should contain %q", s)
			}
			for _, s := range tc.userNotContain {
				assert.NotContains(t, user, s, "user prompt should not contain %q", s)
			}
			for _, s := range tc.sysContains {
				assert.Contains(t, sys, s, "system prompt should contain %q", s)
			}
		})
	}
}

func TestBuildTestGenPrompt(t *testing.T) {
	sys, user := BuildTestGenPrompt("intent-x", "gh", "<yaml>")
	assert.Contains(t, user, "intent-x", "user prompt should contain intent")
	assert.Contains(t, user, "<yaml>", "user prompt should contain toolkit yaml")
	assert.Contains(t, sys, "testCases", "system prompt should mention testCases schema")
}

func TestParseTestGenResponse(t *testing.T) {
	raw := `{"testCases":[
  {"intent":"allow view","argv":["pr","view"],"expectAllow":true},
  {"intent":"deny merge","argv":["pr","merge"],"expectAllow":false,"env":{"GITHUB_TOKEN":"x"}}
]}`
	r, err := ParseTestGenResponse(raw)
	require.NoError(t, err, "ParseTestGenResponse")
	require.Len(t, r.TestCases, 2, "TestCases length")
	assert.Equal(t, "x", r.TestCases[1].Env["GITHUB_TOKEN"], "TestCases[1].Env[GITHUB_TOKEN]")
}

// --- array-wrapping tolerance -----------------------------------------------
// Gemini occasionally wraps the response JSON in a one-element array. The
// parsers unwrap that transparently; multi-element arrays and empty arrays
// still surface a clear error rather than silently accepting nonsense.

func TestUnwrapSingleArray(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "object passthrough returns object", in: `{"x":1}`, want: `{"x":1}`},
		{name: "single-array unwrap yields inner object", in: `[{"x":1}]`, want: `{"x":1}`},
		{name: "single-array with surrounding whitespace unwraps", in: "  [ {\"x\":1} ]  ", want: `{"x":1}`},
		{name: "multi-array passthrough returns array unchanged", in: `[{"x":1},{"x":2}]`, want: `[{"x":1},{"x":2}]`},
		{name: "empty-array passthrough returns array unchanged", in: `[]`, want: `[]`},
		{name: "non-json passthrough returns input unchanged", in: `not json at all`, want: `not json at all`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, UnwrapSingleArray(tc.in), "UnwrapSingleArray(%q)", tc.in)
		})
	}
}

func TestParseGenerateResponse_ArrayWrapped(t *testing.T) {
	raw := `[
  {
    "specYAML": "name: s\nversion: \"1\"\n",
    "descriptions": {"allowSubcommands[0]": "desc"},
    "warnings": [],
    "excluded": [],
    "unmatched": []
  }
]`
	r, err := ParseGenerateResponse(raw)
	require.NoError(t, err, "array-wrapped response should parse")
	assert.Contains(t, r.SpecYAML, "name: s", "SpecYAML")
	assert.Equal(t, "desc", r.Descriptions["allowSubcommands[0]"], "Descriptions")
}

func TestParseSelectResponse_ArrayWrapped(t *testing.T) {
	raw := `[{"toolkitName":"gh","reasoning":"matches","unmatched":[]}]`
	r, err := ParseSelectResponse(raw)
	require.NoError(t, err, "array-wrapped response should parse")
	assert.Equal(t, "gh", r.ToolkitName, "ToolkitName")
}

func TestParseTestGenResponse_ArrayWrapped(t *testing.T) {
	raw := `[{"testCases":[{"intent":"x","argv":["pr","view"],"expectAllow":true}]}]`
	r, err := ParseTestGenResponse(raw)
	require.NoError(t, err, "array-wrapped response should parse")
	require.Len(t, r.TestCases, 1, "TestCases length")
	assert.Equal(t, "x", r.TestCases[0].Intent, "TestCases[0].Intent")
}

func TestParseGenerateResponse_MultiElementArrayStillErrors(t *testing.T) {
	// A two-element array should NOT be silently accepted; the caller should
	// see a clear parse error rather than guessing at which element to take.
	raw := `[{"specYAML":"a"},{"specYAML":"b"}]`
	_, err := ParseGenerateResponse(raw)
	assert.Error(t, err, "multi-element array should error")
}

// Also verify the fencing + array combo ("```json\n[{...}]\n```") works end to end.
func TestParseGenerateResponse_FencedArray(t *testing.T) {
	raw := "```json\n[{\"specYAML\":\"name: s\\nversion: \\\"1\\\"\\n\"}]\n```"
	r, err := ParseGenerateResponse(raw)
	require.NoError(t, err, "fenced-array should parse")
	assert.Contains(t, r.SpecYAML, "name: s", "SpecYAML")
}
