// Package assets embeds the pre-rendered menu-bar icon PNG frames produced by
// `mage desktop:icons`. Kept separate from the render package so the shipped
// oap binary links only these bytes, never the rasterizer.
package assets

import (
	"embed"
	"fmt"
	"sync"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/menubaricons"
)

//go:embed *.png
var files embed.FS

var (
	once    sync.Once
	frames  map[menubaricons.State][][]byte
	loadErr error
)

// Frames returns the PNG frames for a state, index 0..N-1. It panics if the
// embedded assets are missing or inconsistent with the state table — a
// build-time (generator) bug the golden test guards against.
func Frames(s menubaricons.State) [][]byte {
	once.Do(load)
	if loadErr != nil {
		panic(loadErr)
	}
	return frames[s]
}

func load() {
	frames = map[menubaricons.State][][]byte{}
	for _, s := range menubaricons.AllStates() {
		fs := make([][]byte, s.FrameCount())
		for i := 0; i < s.FrameCount(); i++ {
			name := fmt.Sprintf("%s_%02d.png", s.Basename(), i)
			b, err := files.ReadFile(name)
			if err != nil {
				loadErr = fmt.Errorf("menubaricons assets: %w", err)
				return
			}
			fs[i] = b
		}
		frames[s] = fs
	}
}
