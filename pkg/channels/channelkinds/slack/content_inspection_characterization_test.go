// pkg/channels/channelkinds/slack/content_inspection_characterization_test.go
//
// Characterization pin for inertExcerpt (inert.go), the helper the interaction
// renderer runs over an untrusted excerpt before placing it inside a code
// fence. The renderer-level invariant it feeds — headline plus code-fenced
// inert excerpt — is pinned separately by
// TestBuildInteractionRequestBlocks_RendersInertExcerpt in
// interaction_excerpt_test.go.
package slack

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestContentInspectionInertExcerpt_Characterization pins the security-critical
// inert treatment of the untrusted excerpt: backtick triplets neutralized (so
// the excerpt cannot break the surrounding code fence) and & < > HTML-escaped
// (so <!channel>/<!here> pings and tags never fire).
func TestContentInspectionInertExcerpt_Characterization(t *testing.T) {
	require.Equal(t, "ʼʼʼnot a fence", inertExcerpt("```not a fence"),
		"backtick triplets must be neutralized to modifier apostrophes")
	require.Equal(t, "&lt;!channel&gt; &amp; &lt;tag&gt;", inertExcerpt("<!channel> & <tag>"),
		"& < > must be HTML-escaped")
}
