package kindtest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
)

// TestExcerptInertness_TextFloor asserts the Excerpt-inertness contract on the
// CommonMark text floor: the SampleRequest excerpt is captured inside a code
// fence strictly longer than any backtick run the excerpt itself contains — so
// its own content cannot close the fence early — and the excerpt's Label AND
// Content both sit inside that fenced region, never adjacent to the rendered
// Actions as live markup.
//
// It is deliberately NOT a literal NotContains of the raw excerpt: RenderText
// widens the fence rather than escaping, so the raw bytes intentionally remain
// present in the output — they are simply no longer live. Escaping is the
// Slack-mrkdwn technique, asserted separately on that dialect via
// kindtest.AssertExcerptInert, because CommonMark fenced blocks guarantee their
// content is never reinterpreted once the fence is wide enough.
//
// A new category with an Excerpt auto-extends this registry-wide check.
func TestExcerptInertness_TextFloor(t *testing.T) {
	for _, c := range channelinteractions.All() {
		c := c
		t.Run("category_"+c.Name, func(t *testing.T) {
			req := SampleRequest(c)
			if req.Excerpt == nil {
				t.Skip("category has no excerpt")
			}
			out := channelinteractions.RenderText(req, nil)

			// Find the longest backtick run in the whole rendered output — that
			// run is the wrapping fence. It must exist, and its opening/closing
			// occurrences must bracket a region containing the excerpt's label
			// and content: the load-bearing guarantee that the excerpt (and any
			// fence-breaking backticks it carries) never escapes to become live
			// markdown adjacent to the rendered lead/actions.
			longest, run := 0, 0
			for _, r := range out {
				if r == '`' {
					run++
					if run > longest {
						longest = run
					}
				} else {
					run = 0
				}
			}
			require.Greater(t, longest, 0, "category %q: excerpt must be code-fenced", c.Name)
			fence := strings.Repeat("`", longest)
			first := strings.Index(out, fence)
			last := strings.LastIndex(out, fence)
			require.Greater(t, last, first, "category %q: output carries a distinct opening and closing fence", c.Name)
			fenced := out[first : last+len(fence)]

			assert.Contains(t, fenced, req.Excerpt.Content,
				"raw fence-breaking excerpt content must stay INSIDE the fenced region")
			if req.Excerpt.Label != "" {
				assert.Contains(t, fenced, req.Excerpt.Label,
					"excerpt label must stay INSIDE the fenced region, never live outside it")
			}
		})
	}
}
