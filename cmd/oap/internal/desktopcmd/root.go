// Package desktopcmd implements `oap desktop`, the macOS menubar app: a local
// VM running a single-node cluster, the setup window that brings it up, and
// the .oap install flow a double-clicked bundle takes. Everything but the
// command wiring is macOS/Apple-Silicon only; other platforms link the stub.
package desktopcmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

// NewCmd registers `oap desktop` — the macOS menubar app that drives a
// local, single-user agent-primitives deployment: it boots a lightweight
// Linux VM (Apple Virtualization.framework), installs agent-primitives into
// it, and gives the user a menu to open the built-in chat / dashboard, stop,
// or uninstall.
//
// This file carries NO build tag: it wires the cobra command on every
// platform so `oap --help` lists `desktop` consistently. The actual
// implementation is platform-specific (see runDesktop in desktop_darwin.go /
// desktop_stub.go) — cgo (fyne.io/systray + Code-Hex/vz) is only pulled in on
// darwin/arm64, so `CGO_ENABLED=0 GOOS=linux go build ./cmd/oap` still builds
// clean (desktop_darwin.go is excluded by its build tag; desktop_stub.go's
// runDesktop just returns an unsupported-platform error).
func NewCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "desktop",
		Short: "Run the oap macOS menubar app (local VM + built-in chat; macOS/Apple Silicon only)",
		Long: `desktop drives a local, single-user agent-primitives deployment from a
menubar app: it boots a lightweight Linux VM (Virtualization.framework),
installs agent-primitives into it with a persistent SQLite memory backend,
and gives you a menu to open the built-in chat, open the admin dashboard,
stop the VM, or uninstall.

Only supported on macOS (Apple Silicon). On other platforms this command
returns an error explaining why.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDesktop(cmd.Context(), cmd.OutOrStdout(), g)
		},
	}
}
