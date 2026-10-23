package mdxutil

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

func TestSummary(t *testing.T) {
	long := strings.Repeat("word ", 50) + "end."
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "wrapped first sentence: joined, stops at the period",
			in:   "AgentClass is the template: model, system\nprompt, tools. It never runs\non its own.",
			want: "AgentClass is the template: model, system prompt, tools.",
		},
		{
			name: "single sentence: returned whole",
			in:   "SkillSource is a git repo that yields one or more Skills.",
			want: "SkillSource is a git repo that yields one or more Skills.",
		},
		{
			name: "second paragraph: ignored",
			in:   "Widget is a demo\n\nNamespaced. Reconciled elsewhere.",
			want: "Widget is a demo",
		},
		{
			name: "abbreviation before lowercase: not a sentence end",
			in:   "Channel binds a transport, e.g. a Slack channel. More here.",
			want: "Channel binds a transport, e.g. a Slack channel.",
		},
		{
			name: "over the limit: cut at a word boundary with an ellipsis",
			in:   long,
			want: strings.TrimSpace(strings.Repeat("word ", 32)) + "…",
		},
		{
			// 159 ASCII bytes, then a 3-byte rune straddling the 160-byte limit.
			name: "no space and a rune across the limit: cut before the rune, valid UTF-8",
			in:   strings.Repeat("a", 159) + "日本語",
			want: strings.Repeat("a", 159) + "…",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Summary(tc.in)
			assert.True(t, utf8.ValidString(got), "valid UTF-8")
			assert.Equal(t, tc.want, got)
			assert.LessOrEqual(t, len(strings.TrimSuffix(got, "…")), summaryMax)
		})
	}
}
