package validate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSkill(t *testing.T) {
	const okCanonical = "github.com/someorg/somerepo//skills/skillone"
	cases := []struct {
		name        string
		canonical   string
		fmName      string
		description string
		body        string
		wantErr     bool
	}{
		{name: "all valid", canonical: okCanonical, fmName: "skillone",
			description: "Use when formatting invoices.", body: "Do the thing.", wantErr: false},
		{name: "bad canonical name", canonical: "not-a-canonical-name", fmName: "skillone",
			description: "x", body: "y", wantErr: true},
		{name: "frontmatter name not kebab", canonical: okCanonical, fmName: "Skill_One",
			description: "x", body: "y", wantErr: true},
		{name: "frontmatter name too long", canonical: okCanonical, fmName: strings.Repeat("a", 65),
			description: "x", body: "y", wantErr: true},
		{name: "name does not match dir", canonical: okCanonical, fmName: "other",
			description: "x", body: "y", wantErr: true},
		{name: "reserved word in name", canonical: "github.com/o/r//claude-helper", fmName: "claude-helper",
			description: "x", body: "y", wantErr: true},
		{name: "empty description", canonical: okCanonical, fmName: "skillone",
			description: "", body: "y", wantErr: true},
		{name: "description too long", canonical: okCanonical, fmName: "skillone",
			description: strings.Repeat("x", 1025), body: "y", wantErr: true},
		{name: "xml in description", canonical: okCanonical, fmName: "skillone",
			description: "see <tag>here</tag>", body: "y", wantErr: true},
		{name: "reserved word in description", canonical: okCanonical, fmName: "skillone",
			description: "wraps the Anthropic API", body: "y", wantErr: true},
		{name: "empty body", canonical: okCanonical, fmName: "skillone",
			description: "x", body: "   ", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := Skill(tc.canonical, tc.fmName, tc.description, tc.body)
			if tc.wantErr {
				assert.NotEmpty(t, errs)
			} else {
				assert.Empty(t, errs)
			}
		})
	}
}

func TestCheckProvenance(t *testing.T) {
	cases := []struct {
		name         string
		canonical    string
		materialized bool
		wantErr      bool
	}{
		{name: "hand-authored local name: ok", canonical: "local//myskill", materialized: false, wantErr: false},
		{name: "hand-authored namespaced local name: ok", canonical: "local/ns//myskill", materialized: false, wantErr: false},
		{name: "hand-authored git-authority name: rejected (spoof)", canonical: "github.com/trusted-org/repo//skills/x@v1", materialized: false, wantErr: true},
		{name: "materialized git-authority name: ok", canonical: "github.com/trusted-org/repo//skills/x@v1", materialized: true, wantErr: false},
		{name: "materialized local name: ok", canonical: "local//myskill", materialized: true, wantErr: false},
		{name: "unparseable name: parse error surfaced", canonical: "not-canonical", materialized: false, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckProvenance(tc.canonical, tc.materialized)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
