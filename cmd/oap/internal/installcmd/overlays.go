package installcmd

import (
	"io"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/cliout"
	"github.com/authzed/openagentprimitives/pkg/platform/manifests/overlay"
)

const overlayFlagUsage = "Layer a local kustomize Component directory over the installed manifests (repeatable; applied in order). " +
	"Use it for changes that must survive re-installs and upgrades, such as a raised memory limit. " +
	"Overlays that set the same field to different values are refused before anything is installed"

// loadOverlays validates the --overlay directories against every manifest oap
// ships and says which resources they change. It runs before any cluster call,
// so a broken or conflicting overlay costs nothing. No dirs returns a nil Set,
// which applies nothing.
func loadOverlays(out io.Writer, dirs []string) (*overlay.Set, error) {
	if len(dirs) == 0 {
		return nil, nil
	}
	base, err := overlay.Base()
	if err != nil {
		return nil, err
	}
	s, err := overlay.Load(base, dirs)
	if err != nil {
		return nil, err
	}
	patched := s.Patched()
	if len(patched) == 0 {
		cliout.Warn(out, "overlays %v change nothing oap installs", dirs)
		return s, nil
	}
	for _, id := range patched {
		cliout.Info(out, "overlay patches %s", id)
	}
	return s, nil
}
