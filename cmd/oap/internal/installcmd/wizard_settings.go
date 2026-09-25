package installcmd

import (
	"context"
	"io"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/settingscmd"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// runSettingsWizardFn runs the security-settings wizard over an injected driver.
// A package var — mirroring runDirectoryConfigureFn (init.go) — so a same-package
// test can substitute a spy that proves runSettingsReframed reframes and
// forwards the shared driver, without standing up a cluster.
var runSettingsWizardFn = settingscmd.RunWizardWithDriver

// settingsReframeParams bundles what the settings phase of the unified init
// wizard passes through to settingscmd: the recommended-baseline toggle and the
// build outputs the model catalog's image defaults derive from. Task 11-B, which
// owns the orchestrator, populates it from runInit's flags and the digests
// runBuild returned.
type settingsReframeParams struct {
	defaults bool
	registry string
	digests  map[string]string
	dm       settingscmd.DefaultModelOpts
}

// runSettingsReframed runs the security-settings wizard as one phase of the
// unified init wizard. It reframes the run's single shared driver onto the
// Settings rail step (keySettings) so the settings screens present under the
// same chrome/rail as every other phase, then runs settingscmd's wizard over
// that reframed driver. A throwaway cobra.Command carries out as the wizard's
// output stream, mirroring runInitWizardOffer (init.go).
func runSettingsReframed(ctx context.Context, out io.Writer, g *apcmd.Globals, chrome *tui.Chrome, driver tui.Driver, p settingsReframeParams) error {
	fakeCmd := &cobra.Command{}
	fakeCmd.SetOut(out)
	reframed := tui.Reframe(driver, chrome, keySettings)
	return runSettingsWizardFn(ctx, fakeCmd, g, p.defaults, false /*dryRun*/, p.registry, p.digests, p.dm, reframed)
}
