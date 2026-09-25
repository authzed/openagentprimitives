// Command gen renders the oap desktop menu-bar icon frames to PNG assets. Run
// via `mage desktop:icons`. Not part of the shipped binary.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/menubaricons"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop/menubaricons/render"
)

func main() {
	out := flag.String("out", "cmd/oap/internal/desktop/menubaricons/assets", "output directory for PNG frames")
	flag.Parse()
	if err := run(*out); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run(out string) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	for _, s := range menubaricons.AllStates() {
		frames := render.RenderFrames(s)
		for i, b := range frames {
			name := fmt.Sprintf("%s_%02d.png", s.Basename(), i)
			if err := os.WriteFile(filepath.Join(out, name), b, 0o644); err != nil {
				return fmt.Errorf("write %s: %w", name, err)
			}
		}
		fmt.Printf("gen: %s (%d frame(s))\n", s.Basename(), len(frames))
	}
	return nil
}
