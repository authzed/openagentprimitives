package installcmd

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/settingscmd"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// reframeSpyDriver is a Reframer that records the rail step it was reframed to
// and returns a distinct driver, so the test can prove runSettingsReframed both
// reframes onto keySettings and forwards the reframed driver into the wizard.
type reframeSpyDriver struct {
	reframedTo string
	reframed   tui.Driver
}

func (d *reframeSpyDriver) Present(context.Context, string, *huh.Group) error { return nil }

func (d *reframeSpyDriver) Reframe(_ *tui.Chrome, stepID string) tui.Driver {
	d.reframedTo = stepID
	return d.reframed
}

// TestRunSettingsReframed_ReframesAndForwardsSharedDriver pins 11e: the settings
// phase reframes the run's ONE shared driver onto the Settings rail step and
// hands that reframed driver to settingscmd — not a fresh one. Mirrors
// directorycmd's TestRunWizard_BuildsOneDriverSharedAcrossPhases in intent: the
// shared driver is what actually reaches the wizard.
func TestRunSettingsReframed_ReframesAndForwardsSharedDriver(t *testing.T) {
	theme := tui.NewTheme(tui.Caps{})
	reframed := tui.Plain(strings.NewReader(""), io.Discard, theme) // the distinct driver Reframe returns
	shared := &reframeSpyDriver{reframed: reframed}
	chrome := tui.NewChrome("oap init", []tui.Step{{ID: keySettings, Label: "Settings"}}, theme)

	var (
		gotDriver   tui.Driver
		gotDefaults bool
		gotRegistry string
		gotDigests  map[string]string
		calls       int
	)
	prev := runSettingsWizardFn
	t.Cleanup(func() { runSettingsWizardFn = prev })
	runSettingsWizardFn = func(_ context.Context, cmd *cobra.Command, _ *apcmd.Globals, defaults, _ bool, registry string, digests map[string]string, _ settingscmd.DefaultModelOpts, driver tui.Driver) error {
		calls++
		gotDriver = driver
		gotDefaults = defaults
		gotRegistry = registry
		gotDigests = digests
		return nil
	}

	err := runSettingsReframed(context.Background(), io.Discard, &apcmd.Globals{}, chrome, shared, settingsReframeParams{
		defaults: true,
		registry: "reg.example.com",
		digests:  map[string]string{"operator": "sha256:abc"},
	})
	require.NoError(t, err)

	require.Equal(t, 1, calls, "the settings wizard must run exactly once")
	assert.Equal(t, keySettings, shared.reframedTo, "the shared driver is reframed onto the Settings rail step")
	assert.Same(t, reframed, gotDriver,
		"the reframed shared driver — not a fresh one — reaches settingscmd")
	assert.True(t, gotDefaults, "params.defaults is forwarded")
	assert.Equal(t, "reg.example.com", gotRegistry)
	assert.Equal(t, map[string]string{"operator": "sha256:abc"}, gotDigests)
}
