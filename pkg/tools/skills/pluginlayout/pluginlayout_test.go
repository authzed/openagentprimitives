package pluginlayout

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNameValidation(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantName  bool
		wantAgent bool
	}{
		{name: "kebab-case: valid for both", in: "bug-hunter", wantName: true, wantAgent: true},
		{name: "underscore or capitals: agent only", in: "Bug_Hunter", wantName: false, wantAgent: true},
		{name: "empty: neither", in: ""},
		{name: "path separator: neither", in: "a/b"},
		{name: "parent traversal: neither", in: ".."},
		{name: "leading dot: neither", in: ".hidden"},
		{name: "dot inside: neither", in: "a.b"},
		{name: "over the length limit: neither", in: strings.Repeat("a", MaxNameLen+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantName, ValidName(tc.in), "ValidName")
			assert.Equal(t, tc.wantAgent, ValidAgentName(tc.in), "ValidAgentName")
		})
	}
}

func TestIsReserved(t *testing.T) {
	cases := []struct {
		rel  string
		want bool
	}{
		{rel: Dir, want: true},
		{rel: ManifestPath(), want: true},
		{rel: AgentPath("x"), want: true},
		{rel: Dir + "-not/x", want: false},
		{rel: "scripts/" + Dir + "/x", want: false},
		{rel: "SKILL.md", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.rel, func(t *testing.T) {
			assert.Equal(t, tc.want, IsReserved(tc.rel))
		})
	}
}
