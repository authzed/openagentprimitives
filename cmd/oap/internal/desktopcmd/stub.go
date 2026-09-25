//go:build !(darwin && arm64)

package desktopcmd

import (
	"context"
	"fmt"
	"io"
	"runtime"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

// runDesktop on every platform other than darwin/arm64: there is no
// Virtualization.framework (or equivalent) to boot a guest against, so
// `oap desktop` fails closed with a clear, actionable error rather than
// silently doing nothing. Keeping this stub in its own !(darwin && arm64)
// file — rather than an inline platform check inside desktop_darwin.go — is
// what lets `CGO_ENABLED=0 GOOS=linux go build ./cmd/oap` build cleanly: the
// cgo-heavy real implementation (fyne.io/systray, Code-Hex/vz) never even
// gets compiled on this path.
func runDesktop(_ context.Context, _ io.Writer, _ *apcmd.Globals) error {
	return fmt.Errorf("OAP Desktop is only supported on macOS (Apple Silicon); this binary was built for %s/%s", runtime.GOOS, runtime.GOARCH)
}
