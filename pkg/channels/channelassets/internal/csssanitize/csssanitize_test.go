package csssanitize_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/channels/channelassets/internal/csssanitize"
)

func TestSanitize(t *testing.T) {
	cases := []struct {
		name         string
		in           string
		wantOut      string // "" means "expect byte-identical to in"
		wantStripped []string
		wantCount    map[string]int // optional: assert per-construct aggregate counts
	}{
		{
			name:    "modern CSS preserved byte-identical",
			in:      ":root{--bg:#0b0d12}.a>.b+.c{color:var(--bg);width:clamp(1px,2vw,3px)}@media(max-width:600px){.g{display:grid}}",
			wantOut: "",
		},
		{
			name:    "data: url preserved",
			in:      ".x{background:url(data:image/png;base64,iVBORw0KGgo=)}",
			wantOut: "",
		},
		{
			name:    "fragment url preserved",
			in:      ".x{filter:url(#blur)}",
			wantOut: "",
		},
		{
			name:    "keyframes and font-face preserved",
			in:      "@keyframes spin{from{transform:rotate(0)}to{transform:rotate(360deg)}}@font-face{font-family:x;src:url(data:font/woff2;base64,AAA)}",
			wantOut: "",
		},
		{
			name:    "selector named .behavior is NOT a banned property",
			in:      ".behavior{color:red}.expression{color:blue}",
			wantOut: "",
		},
		{
			name:    "custom property value left verbatim",
			in:      ":root{--x:url(https://example.com/a.png)}",
			wantOut: "", // accepted gap: not scanned inside a custom-property value
		},
		{
			name:         "remote url stripped",
			in:           ".x{background:url(https://evil.example/p.png)}",
			wantOut:      ".x{background:}",
			wantStripped: []string{"url()"},
		},
		{
			name:         "quoted remote url stripped",
			in:           `.x{background:url("https://evil.example/p.png")}`,
			wantOut:      ".x{background:}",
			wantStripped: []string{"url()"},
		},
		{
			name:         "@import stripped",
			in:           `@import url("https://evil.example/x.css");.a{color:red}`,
			wantOut:      `.a{color:red}`,
			wantStripped: []string{"@import"},
		},
		{
			name:         "expression() stripped",
			in:           ".x{width:expression(alert(1))}",
			wantOut:      ".x{width:}",
			wantStripped: []string{"expression()"},
		},
		{
			name:         "behavior property stripped",
			in:           ".x{behavior:url(xss.htc);color:red}",
			wantOut:      ".x{color:red}",
			wantStripped: []string{"behavior"},
		},
		{
			name:         "javascript url stripped",
			in:           ".x{background:url(javascript:alert(1))}",
			wantOut:      ".x{background:}",
			wantStripped: []string{"url()"},
		},
		{
			name:         "behavior as last declaration (no trailing semicolon) stripped, brace kept",
			in:           ".x{color:red;behavior:url(xss.htc)}",
			wantOut:      ".x{color:red;}",
			wantStripped: []string{"behavior"},
		},
		{
			name:         "-moz-binding property stripped",
			in:           ".x{-moz-binding:url(xss.xml#e);color:red}",
			wantOut:      ".x{color:red}",
			wantStripped: []string{"-moz-binding"},
		},
		{
			name:         "remote url inside @media is stripped but @media preserved",
			in:           "@media(max-width:600px){.g{background:url(https://evil.example/p.png)}}",
			wantOut:      "@media(max-width:600px){.g{background:}}",
			wantStripped: []string{"url()"},
		},
		{
			name:         "two remote urls in same rule aggregate to count==2",
			in:           ".x{background:url(https://evil.example/a.png);border-image:url(https://evil.example/b.png)}",
			wantOut:      ".x{background:;border-image:}",
			wantStripped: []string{"url()"},
			wantCount:    map[string]int{"url()": 2},
		},
		{
			name:    "empty input",
			in:      "",
			wantOut: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, warns := csssanitize.Sanitize(tc.in)
			want := tc.wantOut
			if want == "" {
				want = tc.in
			}
			assert.Equal(t, want, got, "sanitized CSS")
			var names []string
			gotCount := map[string]int{}
			for _, w := range warns {
				assert.Equal(t, "css", w.Kind)
				assert.Equal(t, "stripped", w.Action)
				names = append(names, w.Name)
				gotCount[w.Name] += w.Count
			}
			assert.ElementsMatch(t, tc.wantStripped, names, "stripped construct names")
			for name, want := range tc.wantCount {
				assert.Equal(t, want, gotCount[name], "aggregate count for %s", name)
			}
		})
	}
}
