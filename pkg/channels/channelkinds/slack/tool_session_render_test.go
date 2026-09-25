package slack

import (
	"fmt"
	"strings"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatElapsed(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{45 * time.Second, "45s"},
		{83 * time.Second, "1m23s"},
		{90 * time.Second, "1m30s"},
		{3723 * time.Second, "1h02m"},
		{-5 * time.Second, "0s"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, formatElapsed(tc.in), "elapsed %v", tc.in)
	}
}

func TestCodeBlockTail(t *testing.T) {
	// Short input is returned unchanged — no marker, nothing to trim.
	assert.Equal(t, "small output", codeBlockTail("small output"))
	assert.Equal(t, "line1\nline2\nline3", codeBlockTail("line1\nline2\nline3"))

	// Surrounding blank lines / whitespace are trimmed so the code block
	// has no leading or trailing empty line.
	assert.Equal(t, "a\nb", codeBlockTail("\n\n  a\nb  \n\n"))

	// More than toolSessionTailLines lines → only the last N are kept,
	// with the truncation marker prefixed.
	var many []string
	for i := 0; i < toolSessionTailLines+8; i++ {
		many = append(many, fmt.Sprintf("line%d", i))
	}
	got := codeBlockTail(strings.Join(many, "\n"))
	require.True(t, strings.HasPrefix(got, "…(earlier output truncated)\n"), "marker prefix")
	body := strings.TrimPrefix(got, "…(earlier output truncated)\n")
	assert.Len(t, strings.Split(body, "\n"), toolSessionTailLines, "exactly N lines kept")
	assert.True(t, strings.HasSuffix(body, fmt.Sprintf("line%d", toolSessionTailLines+8-1)), "newest line kept")
	assert.NotContains(t, body, "line0", "oldest line dropped")

	// A single pathologically long line hits the byte backstop.
	long := codeBlockTail(strings.Repeat("x", toolSessionTailMax+500))
	assert.True(t, strings.HasPrefix(long, "…(earlier output truncated)\n"))
	assert.LessOrEqual(t, len(long), toolSessionTailMax+len("…(earlier output truncated)\n"))
}

// blockText flattens a section / context / rich_text block's text for
// assertions.
func blockText(t *testing.T, b slackapi.Block) string {
	t.Helper()
	switch v := b.(type) {
	case *slackapi.SectionBlock:
		require.NotNil(t, v.Text)
		return v.Text.Text
	case *slackapi.ContextBlock:
		var sb strings.Builder
		for _, el := range v.ContextElements.Elements {
			if tb, ok := el.(*slackapi.TextBlockObject); ok {
				sb.WriteString(tb.Text)
			}
		}
		return sb.String()
	case *slackapi.RichTextBlock:
		var sb strings.Builder
		for _, el := range v.Elements {
			pre, ok := el.(*slackapi.RichTextPreformatted)
			if !ok {
				continue
			}
			for _, se := range pre.Elements {
				if te, ok := se.(*slackapi.RichTextSectionTextElement); ok {
					sb.WriteString(te.Text)
				}
			}
		}
		return sb.String()
	default:
		t.Fatalf("unexpected block type %T", b)
		return ""
	}
}

func TestRenderToolSessionBlocks(t *testing.T) {
	cases := []struct {
		name        string
		view        toolSessionView
		wantHeader  []string // substrings expected in the header block
		wantStatus  string   // exact status-line text
		wantCodeHas string   // substring expected in the code block
	}{
		{
			name: "running with reason",
			view: toolSessionView{toolName: "claude", reason: "write the README",
				transcript: "Reading caucus.py\n", elapsed: 83 * time.Second},
			wantHeader:  []string{"▶️", "`claude`", "write the README"},
			wantStatus:  "running · 1m23s",
			wantCodeHas: "Reading caucus.py",
		},
		{
			name: "finished ok with cost",
			view: toolSessionView{toolName: "claude", reason: "write the README",
				transcript: "done\n", elapsed: 90 * time.Second, finished: true, ok: true, costUSD: 0.0421},
			wantHeader: []string{"✅", "`claude`"},
			wantStatus: "done · $0.0421 · 1m30s",
		},
		{
			name: "finished failed no cost",
			view: toolSessionView{toolName: "claude", transcript: "boom\n",
				elapsed: 90 * time.Second, finished: true, ok: false},
			wantHeader: []string{"❌", "`claude`"},
			wantStatus: "failed · 1m30s",
		},
		{
			name:       "empty tool name falls back to a label",
			view:       toolSessionView{transcript: "x", elapsed: time.Second},
			wantHeader: []string{"▶️", "tool session"},
			wantStatus: "running · 1s",
		},
		{
			name:        "empty transcript shows placeholder",
			view:        toolSessionView{toolName: "claude", elapsed: time.Second},
			wantHeader:  []string{"`claude`"},
			wantStatus:  "running · 1s",
			wantCodeHas: "(no output yet)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocks := renderToolSessionBlocks(tc.view)
			require.Len(t, blocks, 3, "header + status + code")
			header := blockText(t, blocks[0])
			for _, want := range tc.wantHeader {
				assert.Contains(t, header, want)
			}
			assert.Equal(t, tc.wantStatus, blockText(t, blocks[1]))
			code := blockText(t, blocks[2])
			if tc.wantCodeHas != "" {
				assert.Contains(t, code, tc.wantCodeHas)
			}
			// The output block is a rich_text preformatted block tagged
			// "shell" so Slack labels it "Shell".
			rt, ok := blocks[2].(*slackapi.RichTextBlock)
			require.True(t, ok, "code block must be a rich_text block")
			require.Len(t, rt.Elements, 1)
			pre, ok := rt.Elements[0].(*slackapi.RichTextPreformatted)
			require.True(t, ok, "rich_text element must be preformatted")
			assert.Equal(t, "shell", pre.Language)
		})
	}
}
