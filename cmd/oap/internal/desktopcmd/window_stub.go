//go:build !(darwin && arm64)

package desktopcmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/desktop"
)

// NewWindowCmd registers the SAME hidden `desktop-window` command on
// every platform other than darwin/arm64 (so the cobra command tree is
// uniform), but its Run fails closed: webview_go is cgo and only linked on
// darwin/arm64 (see desktop_window_darwin.go), so keeping the real
// implementation out of this file is what lets `CGO_ENABLED=0 GOOS=linux go
// build ./cmd/oap` stay clean. `oap desktop` itself already refuses to run on
// this platform (desktop_stub.go), so this subcommand is unreachable via
// the normal spawn path in practice — this only matters for someone
// invoking `oap desktop-window` directly on an unsupported platform.
func NewWindowCmd(_ *apcmd.Globals) *cobra.Command {
	var url, title string
	cmd := &cobra.Command{
		Use:    "desktop-window",
		Short:  "Internal: run the oap desktop setup window (native webview)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return desktop.ErrUnsupportedPlatform
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "URL of the local setup UI server to display (required)")
	cmd.Flags().StringVar(&title, "title", "OAP Desktop", "Window title")
	_ = cmd.MarkFlagRequired("url")
	return cmd
}
