package artifactcmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

// revisionRows is the two-node tree the rendering tests below draw: a root and
// one child carrying both a "latest" tag and an ordinary one, which is the only
// shape that exercises styleTags' two branches at once.
func revisionRows() []artifacts.RevisionView {
	return []artifacts.RevisionView{
		{RevisionID: "artrev-aa", Seq: 1, ChangeDescription: "initial", Tags: nil, MIME: "text/html", Size: 10, CreatedAt: "2026-06-03T00:00:00Z"},
		{RevisionID: "artrev-bb", Seq: 2, ChangeDescription: "added chart", ParentID: "artrev-aa", Tags: []string{artifacts.TagLatest, "published"}, MIME: "text/html", Size: 22, CreatedAt: "2026-06-03T00:01:00Z"},
	}
}

func TestRenderRevisionTree_IncludesSeqAndTags(t *testing.T) {
	var sb strings.Builder
	renderRevisionTree(&sb, tui.NewTheme(tui.Caps{Width: 80}), revisionRows())
	out := sb.String()
	assert.Contains(t, out, "artrev-bb")
	assert.Contains(t, out, "added chart")
	assert.Contains(t, out, "published")
	assert.Contains(t, out, "#2")
}

// A colorless theme is what a pipe, --no-color and NO_COLOR all resolve to, so
// the tree must be byte-clean there — a `oap artifact revisions ... | grep` sees
// exactly the text a terminal shows.
func TestRenderRevisionTree_ColorlessThemeEmitsNoEscapeCodes(t *testing.T) {
	var sb strings.Builder
	renderRevisionTree(&sb, tui.NewTheme(tui.Caps{Width: 80, Color: false}), revisionRows())
	assert.False(t, aptest.HasANSI(sb.String()), "colorless theme must not emit escape codes")
}

func TestStyleTags_ColorOnDistinguishesLatestFromOtherTags(t *testing.T) {
	th := tui.NewTheme(tui.Caps{TTY: true, Color: true, Width: 80})

	latest := styleTags(th, []string{artifacts.TagLatest})
	other := styleTags(th, []string{"published"})

	require.True(t, aptest.HasANSI(latest), "a color-enabled theme must style the latest tag")
	require.True(t, aptest.HasANSI(other), "a color-enabled theme must style an ordinary tag")
	assert.NotEqual(t, aptest.ANSIPrefixOf(latest), aptest.ANSIPrefixOf(other),
		"latest and ordinary tags must not collapse to the same color")
}

// Whatever the theme does with a tag, the tag's own text must survive it: a
// style that swallowed the name would leave the user with an unnamed marker.
func TestStyleTags_KeepsTagTextAtEveryColorSetting(t *testing.T) {
	for _, color := range []bool{false, true} {
		th := tui.NewTheme(tui.Caps{TTY: color, Color: color, Width: 80})
		got := styleTags(th, []string{artifacts.TagLatest, "published"})
		assert.Contains(t, got, "["+artifacts.TagLatest+"]", "color=%v", color)
		assert.Contains(t, got, "[published]", "color=%v", color)
	}
}
