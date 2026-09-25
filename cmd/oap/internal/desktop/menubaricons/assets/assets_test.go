package assets

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/menubaricons"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/menubaricons/render"
)

func TestFramesLoadAllStates(t *testing.T) {
	for _, s := range menubaricons.AllStates() {
		require.Len(t, Frames(s), s.FrameCount(), s.Basename())
		for i, b := range Frames(s) {
			require.NotEmptyf(t, b, "%s frame %d", s.Basename(), i)
		}
	}
}

func TestAssetsMatchGenerator(t *testing.T) {
	for _, s := range menubaricons.AllStates() {
		want := render.RenderFrames(s)
		got := Frames(s)
		require.Lenf(t, got, len(want), "%s frame count", s.Basename())
		for i := range want {
			require.Truef(t, bytes.Equal(want[i], got[i]),
				"asset %s_%02d.png is stale — run `mage desktop:icons`", s.Basename(), i)
		}
	}
}
