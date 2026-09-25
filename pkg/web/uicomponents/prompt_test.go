package uicomponents_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
)

func TestPromptRefs(t *testing.T) {
	cases := []struct {
		name   string
		prompt string
		want   []string
	}{
		{name: "no placeholders: nothing to supply", prompt: "summarize what is on screen", want: nil},
		{name: "one placeholder", prompt: "tell me about {company}", want: []string{"company"}},
		{
			name:   "several, deduplicated and sorted so a document's requirements read the same every time",
			prompt: "compare {b} with {a}, then {a} again",
			want:   []string{"a", "b"},
		},
		{
			// A dotted key is what an ap:daterange actually drives, so this is
			// the ordinary case rather than an exotic one.
			name:   "a dotted parameter key",
			prompt: "companies since {window.from}",
			want:   []string{"window.from"},
		},
		{
			// `{` is a character that legitimately appears in prose. Refusing a
			// document over one would be a worse trade than leaving it in the
			// sentence.
			name:   "an unterminated brace is prose, not an error",
			prompt: "what about {this",
			want:   nil,
		},
		{name: "an empty placeholder names nothing", prompt: "a {} b", want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, uicomponents.PromptRefs(tc.prompt))
		})
	}
}

func TestFillPrompt(t *testing.T) {
	values := map[string]string{"company": "Acme", "window.from": "2026-02-01T00:00:00Z"}

	cases := []struct {
		name   string
		prompt string
		want   string
	}{
		{name: "substitutes a value", prompt: "tell me about {company}", want: "tell me about Acme"},
		{name: "substitutes every occurrence", prompt: "{company} and {company}", want: "Acme and Acme"},
		{name: "substitutes a dotted key", prompt: "since {window.from}", want: "since 2026-02-01T00:00:00Z"},
		{name: "leaves prose alone", prompt: "summarize this", want: "summarize this"},
		{
			// The important one. An unfilled hole left as "{missing}" is
			// visibly broken and gets asked about; replaced with nothing it
			// becomes a grammatical sentence about nothing, which the agent
			// answers confidently and wrongly.
			name:   "leaves an unsupplied placeholder visible rather than emptying it",
			prompt: "tell me about {missing}",
			want:   "tell me about {missing}",
		},
		{name: "an unterminated brace survives verbatim", prompt: "what about {this", want: "what about {this"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, uicomponents.FillPrompt(tc.prompt, values))
		})
	}
}
