package mcp_test

import (
	"testing"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/mcp"
	"github.com/stretchr/testify/assert"
)

// TestRedactSensitive exercises RedactSensitive across its representative
// shapes: top-level scalar paths, nested object paths, paths that don't
// match anything (no-op), deeply-nested leaves, bracketed array indices,
// and Redactor-backed dedupe of identical values across paths.
func TestRedactSensitive(t *testing.T) {
	cases := []struct {
		name   string
		in     map[string]any
		paths  []string
		want   map[string]any
		assert func(t *testing.T, in, got map[string]any)
	}{
		{
			name:  "top-level path: scalar redacted, sibling untouched",
			in:    map[string]any{"a": "x", "b": "y"},
			paths: []string{"a"},
			want:  map[string]any{"a": `<redacted id="1"/>`, "b": "y"},
		},
		{
			name: "nested path: leaf redacted, original input not mutated",
			in: map[string]any{
				"config": map[string]any{"apiKey": "secret", "host": "x"},
			},
			paths: []string{"config.apiKey"},
			want: map[string]any{
				"config": map[string]any{"apiKey": `<redacted id="1"/>`, "host": "x"},
			},
			assert: func(t *testing.T, in, _ map[string]any) {
				assert.Equal(t, "secret", in["config"].(map[string]any)["apiKey"],
					"original input must not be mutated")
			},
		},
		{
			name:  "path matching nothing: returns input as no-op",
			in:    map[string]any{"a": 1},
			paths: []string{"missing"},
			want:  map[string]any{"a": 1},
		},
		{
			name: "deeply-nested path: leaf redacted, siblings up the chain untouched",
			in: map[string]any{
				"args": map[string]any{
					"search": map[string]any{
						"query": "ssn=000-00-0000",
						"limit": float64(10),
					},
					"keep": "untouched",
				},
			},
			paths: []string{"args.search.query"},
			want: map[string]any{
				"args": map[string]any{
					"search": map[string]any{
						"query": `<redacted id="1"/>`,
						"limit": float64(10),
					},
					"keep": "untouched",
				},
			},
		},
		{
			name: "bracketed array index: only targeted element redacted",
			in: map[string]any{
				"args": map[string]any{
					"tags": []any{"a", "b", "secret-c", "d"},
				},
			},
			paths: []string{"args.tags[2]"},
			want: map[string]any{
				"args": map[string]any{
					"tags": []any{"a", "b", `<redacted id="1"/>`, "d"},
				},
			},
			assert: func(t *testing.T, in, _ map[string]any) {
				assert.Equal(t, "secret-c", in["args"].(map[string]any)["tags"].([]any)[2],
					"original input must not be mutated")
			},
		},
		{
			// Regression: the Redactor backs the rewrite, so duplicate sensitive
			// values across paths share the same token rather than minting a new
			// ID per path. The first value gets id=1; the second distinct value
			// gets id=2; a re-occurrence of the first value reuses id=1.
			name:  "duplicate values across paths: dedupes via Redactor id",
			in:    map[string]any{"a": "shared", "b": "other", "c": "shared"},
			paths: []string{"a", "b", "c"},
			want: map[string]any{
				"a": `<redacted id="1"/>`,
				"b": `<redacted id="2"/>`,
				"c": `<redacted id="1"/>`,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mcp.RedactSensitive(tc.in, tc.paths)
			assert.Equal(t, tc.want, got)
			if tc.assert != nil {
				tc.assert(t, tc.in, got)
			}
		})
	}
}
