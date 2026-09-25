package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHtmlToText(t *testing.T) {
	cases := []struct {
		name         string
		html         string
		wantContains []string
		wantNotAny   []string
	}{
		{
			name: "adjacent paragraphs are separated, not glued into one word",
			html: "<p>First paragraph.</p><p>Second paragraph.</p>",
			wantContains: []string{
				"First paragraph.\nSecond paragraph.",
			},
			wantNotAny: []string{"paragraph.Second"},
		},
		{
			name:         "entities are decoded",
			html:         "<p>Fish &amp; Chips</p>",
			wantContains: []string{"Fish & Chips"},
		},
		{
			name:         "script and style content is dropped, not just unwrapped",
			html:         "<html><head><style>.a{color:red}</style><script>alert(1)</script></head><body><p>visible text</p></body></html>",
			wantContains: []string{"visible text"},
			wantNotAny:   []string{"color:red", "alert(1)"},
		},
		{
			name:         "title content is dropped",
			html:         "<html><head><title>Page Title</title></head><body><p>body text</p></body></html>",
			wantContains: []string{"body text"},
			wantNotAny:   []string{"Page Title"},
		},
		{
			name:         "list items each land on their own line",
			html:         "<ul><li>one</li><li>two</li><li>three</li></ul>",
			wantContains: []string{"one\ntwo\nthree"},
		},
		{
			name:         "bold/inline formatting is unwrapped, keeping its text inline",
			html:         "<p>Second with <b>bold</b> text.</p>",
			wantContains: []string{"Second with bold text."},
		},
		{
			name:         "malformed/unclosed tags do not panic and still yield the text",
			html:         "<p>unclosed paragraph <b>bold text",
			wantContains: []string{"unclosed paragraph bold text"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := htmlToText(tc.html)
			for _, want := range tc.wantContains {
				assert.Contains(t, got, want, "got=%q", got)
			}
			for _, notWant := range tc.wantNotAny {
				assert.NotContains(t, got, notWant, "got=%q", got)
			}
		})
	}
}

func TestCollapseBlankLines(t *testing.T) {
	in := "a\n\n\n\nb\n  \nc   \n"
	got := collapseBlankLines(in)
	assert.Equal(t, "a\n\nb\n\nc", got)
}
