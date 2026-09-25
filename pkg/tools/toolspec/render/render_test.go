package render

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/spec"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

func TestDescribe_HandAuthored_Text(t *testing.T) {
	tk, sp := loadPair(t, "hand-authored")
	got, err := Describe(sp, tk, FormatText)
	require.NoError(t, err)
	want := readFile(t, "testdata/hand-authored/expected.text")
	assert.Equal(t, normalize(want), normalize(got))
}

func TestDescribe_LLMDescribed_AllFormats(t *testing.T) {
	tk, sp := loadPair(t, "llm-described")
	for _, format := range []Format{FormatText, FormatMarkdown, FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			got, err := Describe(sp, tk, format)
			require.NoError(t, err)
			want := readFile(t, filepath.Join("testdata", "llm-described", "expected."+string(format)))
			if format == FormatJSON {
				// For json format, assert structural equality, not byte-for-byte.
				var g, w any
				require.NoError(t, json.Unmarshal([]byte(got), &g), "got not json")
				require.NoError(t, json.Unmarshal([]byte(want), &w), "want not json")
				assert.True(t, jsonEqual(g, w), "json mismatch.\n---want---\n%s\n---got---\n%s", want, got)
				return
			}
			assert.Equal(t, normalize(want), normalize(got), "%s mismatch", format)
		})
	}
}

func TestDescribe_UnknownFormat(t *testing.T) {
	tk, sp := loadPair(t, "hand-authored")
	_, err := Describe(sp, tk, Format("bogus"))
	require.Error(t, err, "expected error on unknown format")
}

func TestDescribe_WithMismatches(t *testing.T) {
	tk, _ := loadPair(t, "llm-described")

	actualFalse := false
	sp := &spec.Spec{
		Name:             "test-spec",
		Version:          "1",
		Toolkit:          spec.ToolkitRef{Name: "gh", Revision: "2026-04-24"},
		AllowSubcommands: []string{"pr view"},
		Generation: &spec.Generation{
			Source: "llm:test",
			TestCases: []spec.TestCase{
				{
					Intent:        "Allow viewing a pull request in the authorized authzed/spicedb repository.",
					Argv:          []string{"pr", "view", "42"},
					ExpectAllow:   true,
					LastRunActual: &actualFalse,
					LastRunReason: "denied by constraints[0]: only the authzed/spicedb repo is allowed",
				},
			},
		},
	}

	got, err := Describe(sp, tk, FormatText)
	require.NoError(t, err)
	assert.Contains(t, got, "Allow viewing a pull request in the authorized authzed/spicedb repository.",
		"text output missing mismatch intent")
	assert.Contains(t, got, "expected allow=true, got allow=false",
		"text output missing expected/actual values")
	assert.Contains(t, got, "denied by constraints[0]: only the authzed/spicedb repo is allowed",
		"text output missing mismatch reason")

	gotMD, err := Describe(sp, tk, FormatMarkdown)
	require.NoError(t, err)
	assert.Contains(t, gotMD, "denied by constraints[0]: only the authzed/spicedb repo is allowed",
		"markdown output missing mismatch reason")

	gotJSON, err := Describe(sp, tk, FormatJSON)
	require.NoError(t, err)
	assert.Contains(t, gotJSON, `"reason"`, "json output missing reason field")
	assert.Contains(t, gotJSON, "denied by constraints[0]", "json output missing reason content")
}

// --- helpers ---

func loadPair(t *testing.T, name string) (*toolkit.Toolkit, *spec.Spec) {
	t.Helper()
	tk, err := toolkit.Load(filepath.Join("testdata", name, "toolkit.yaml"))
	require.NoError(t, err, "toolkit.Load")
	sp, err := spec.Load(filepath.Join("testdata", name, "spec.yaml"))
	require.NoError(t, err, "spec.Load")
	return tk, sp
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

func normalize(s string) string {
	return strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
}

func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}
