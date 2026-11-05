package skillmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	const doc = "---\n" +
		"name: skillone\n" +
		"description: Use when formatting invoices.\n" +
		"license: MIT\n" +
		"metadata:\n" +
		"  version: \"1.0\"\n" +
		"---\n" +
		"# Skill One\n\nDo the thing.\n"

	got, err := Parse([]byte(doc))
	require.NoError(t, err)
	assert.Equal(t, "skillone", got.Frontmatter.Name)
	assert.Equal(t, "Use when formatting invoices.", got.Frontmatter.Description)
	assert.Equal(t, "MIT", got.Frontmatter.License)
	assert.Equal(t, "1.0", got.Frontmatter.Metadata["version"])
	assert.Equal(t, "# Skill One\n\nDo the thing.", got.Body)
}

func TestParseErrors(t *testing.T) {
	cases := []struct{ name, in string }{
		{"no frontmatter fence", "name: x\ndescription: y\n"},
		{"unterminated frontmatter", "---\nname: x\n"},
		{"empty", ""},
		{"malformed yaml", "---\nname: : :\n---\nbody"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.in))
			assert.Error(t, err)
		})
	}
}

func TestSplit(t *testing.T) {
	cases := []struct {
		name          string
		in            string
		block, body   string
		noFrontmatter bool
		wantErr       bool
	}{
		{name: "plain fences: block and body split", in: "---\nname: x\n---\nbody\n", block: "name: x", body: "body\n"},
		{name: "CRLF: normalized", in: "---\r\nname: x\r\n---\r\nbody", block: "name: x", body: "body"},
		{name: "leading BOM: still fenced", in: "\ufeff---\nname: x\n---\nbody", block: "name: x", body: "body"},
		{name: "trailing whitespace on fences: still fenced", in: "--- \nname: x\n---\t\nbody", block: "name: x", body: "body"},
		{name: "empty frontmatter: empty block, body kept", in: "---\n---\nbody", block: "", body: "body"},
		{name: "closing fence at EOF: empty body", in: "---\nname: x\n---", block: "name: x", body: ""},
		{name: "a longer dash run is not a closing fence", in: "---\nname: x\n----\n---\nbody", block: "name: x\n----", body: "body"},
		{name: "no opening fence: ErrNoFrontmatter", in: "just a prompt", noFrontmatter: true},
		{name: "unclosed: error", in: "---\nname: x\n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			block, body, err := Split([]byte(tc.in))
			switch {
			case tc.noFrontmatter:
				assert.ErrorIs(t, err, ErrNoFrontmatter)
			case tc.wantErr:
				require.Error(t, err)
				assert.NotErrorIs(t, err, ErrNoFrontmatter)
			default:
				require.NoError(t, err)
				assert.Equal(t, tc.block, block)
				assert.Equal(t, tc.body, body)
			}
		})
	}
}
