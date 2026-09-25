package summarizer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseAndValidate pins the model-output contract: a single JSON
// object with `summary` field, optionally wrapped in markdown fences.
// Anything else (empty body, missing field, non-JSON) is a hard error
// so the caller can fall back to the deterministic "Grant …" line
// rather than render garbage to the approver.
func TestParseAndValidate(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantErr     bool
		wantWords   int
		wantContain string
	}{
		{
			name:        "bare JSON",
			raw:         `{"summary": "Search HubSpot CRM for contacts associated with Acme."}`,
			wantWords:   8,
			wantContain: "Search HubSpot",
		},
		{
			name:        "markdown-fenced JSON",
			raw:         "```json\n{\"summary\": \"Read contacts.\"}\n```",
			wantWords:   2,
			wantContain: "Read contacts.",
		},
		{
			name:        "leading whitespace tolerated",
			raw:         "\n\n   {\"summary\": \"X.\"}   \n",
			wantWords:   1,
			wantContain: "X.",
		},
		{
			name:    "empty summary field is rejected",
			raw:     `{"summary": ""}`,
			wantErr: true,
		},
		{
			name:    "missing summary field is rejected",
			raw:     `{"other": "value"}`,
			wantErr: true,
		},
		{
			name:    "not JSON at all is rejected",
			raw:     `the answer is to search hubspot`,
			wantErr: true,
		},
		{
			name:        "over-30-word summary is truncated with ellipsis",
			raw:         `{"summary": "one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen nineteen twenty twenty-one twenty-two twenty-three twenty-four twenty-five twenty-six twenty-seven twenty-eight twenty-nine thirty thirty-one thirty-two thirty-three"}`,
			wantWords:   30, // ellipsis appended to the 30th word — same word count
			wantContain: "thirty…",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAndValidate(tc.raw)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			words := strings.Fields(got)
			assert.Equal(t, tc.wantWords, len(words), "word count")
			assert.Contains(t, got, tc.wantContain)
		})
	}
}

// TestTruncateStringArgs pins the args-injection-surface-trim
// contract. String values longer than MaxStringArgChars are truncated
// with a "(truncated)" marker so the LLM can see they were cut.
// Numbers/bools/arrays/nested objects pass through unchanged.
func TestTruncateStringArgs(t *testing.T) {
	long := strings.Repeat("x", MaxStringArgChars+50)
	in := `{
		"objectType": "contacts",
		"limit": 50,
		"description": "` + long + `",
		"filters": [{"propertyName": "email", "value": "alice@example.com"}],
		"enabled": true
	}`
	got := truncateStringArgs(in, MaxStringArgChars)

	// Long string truncated, "(truncated)" marker present.
	assert.Contains(t, got, "… (truncated)",
		"long string args must be marked truncated")
	// Short / non-string args passed through.
	assert.Contains(t, got, `"limit":50`)
	assert.Contains(t, got, `"objectType":"contacts"`)
	assert.Contains(t, got, `"enabled":true`)
	assert.Contains(t, got, `"propertyName":"email"`)
	// Length of the truncated args is < the input length.
	assert.Less(t, len(got), len(in),
		"truncated output must be shorter than input with the long string")
}

// TestTruncateStringArgs_InvalidJSONPassesThrough verifies the
// best-effort contract: garbage in → garbage out, but no panic. The
// LLM still sees the original (already-marshalled-by-Go) ArgsJSON.
func TestTruncateStringArgs_InvalidJSONPassesThrough(t *testing.T) {
	got := truncateStringArgs("not actually json", MaxStringArgChars)
	assert.Equal(t, "not actually json", got)
}

// TestCache_GetPut verifies the (tool, args)-keyed memo contract.
// Identical requests hit; differing tool or args miss; over-cap
// inserts evict an earlier entry.
func TestCache_GetPut(t *testing.T) {
	c := NewCache(2)
	r1 := Request{Tool: "tool_a", ArgsJSON: `{"x":1}`}
	r1Same := Request{Tool: "tool_a", ArgsJSON: `{"x":1}`} // same hash
	r2 := Request{Tool: "tool_b", ArgsJSON: `{"x":1}`}     // tool differs
	r3 := Request{Tool: "tool_a", ArgsJSON: `{"x":2}`}     // args differ

	_, ok := c.Get(r1)
	assert.False(t, ok, "miss before put")

	c.Put(r1, "summary 1")
	got, ok := c.Get(r1Same)
	require.True(t, ok, "hit on identical request")
	assert.Equal(t, "summary 1", got)

	_, ok = c.Get(r2)
	assert.False(t, ok, "different tool misses")
	_, ok = c.Get(r3)
	assert.False(t, ok, "different args misses")

	// Cap=2 → third Put evicts one of the earlier entries.
	c.Put(r2, "summary 2")
	c.Put(r3, "summary 3")
	hits := 0
	if _, ok := c.Get(r1); ok {
		hits++
	}
	if _, ok := c.Get(r2); ok {
		hits++
	}
	if _, ok := c.Get(r3); ok {
		hits++
	}
	assert.Equal(t, 2, hits, "cap=2 retains exactly 2 entries after three puts")
}
