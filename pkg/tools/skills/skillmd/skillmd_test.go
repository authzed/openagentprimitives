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
