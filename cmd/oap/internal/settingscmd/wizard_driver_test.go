package settingscmd

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/huh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/settingswizard"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// presentSpy wraps a real Driver and records the ID of every screen actually
// presented THROUGH IT, in order. It is what lets the injected-driver tests
// assert on the property (every screen was served by the injected driver)
// instead of a fingerprint of it. Mirrors directorycmd's presentSpy.
type presentSpy struct {
	real tui.Driver
	seen []string
}

func (s *presentSpy) Present(ctx context.Context, screenID string, g *huh.Group) error {
	s.seen = append(s.seen, screenID)
	return s.real.Present(ctx, screenID, g)
}

// TestUseDriver_KeepsInjectedDriver pins the seam behind RunWizardWithDriver: an
// injected driver survives useDriver rather than being replaced by a fresh one.
// This is the multi-driver bug guard — without it, the settings screens would
// open a second chrome that had never heard of oap init's earlier phases.
func TestUseDriver_KeepsInjectedDriver(t *testing.T) {
	theme := tui.NewTheme(tui.Caps{})
	injected := &presentSpy{real: tui.Plain(strings.NewReader(""), io.Discard, theme)}
	pres := &Presentation{in: strings.NewReader(""), out: io.Discard, theme: theme, driver: injected}

	pres.useDriver([]tui.Step{{ID: "x", Label: "X"}})

	got, ok := pres.driver.(*presentSpy)
	require.True(t, ok, "an injected driver must survive useDriver unchanged")
	assert.Same(t, injected, got)
}

// TestUseDriver_BuildsOneWhenNoneInjected is the nil-driver half: the standalone
// command still gets a driver built for it.
func TestUseDriver_BuildsOneWhenNoneInjected(t *testing.T) {
	pres := &Presentation{in: strings.NewReader(""), out: io.Discard, theme: tui.NewTheme(tui.Caps{})}
	pres.useDriver([]tui.Step{{ID: "x", Label: "X"}})
	assert.NotNil(t, pres.driver, "with no injected driver, useDriver builds one")
}

// TestRunWizardForm_PresentsOverInjectedDriver proves the injected driver is the
// one every screen is actually PRESENTED over — not merely stored. It scripts a
// real off-TTY run (controls, then the prompt-injection screen its answer ticks)
// and asserts both screens' IDs were recorded on the injected spy, in order. If
// useDriver rebuilt its own driver, spy.seen would be empty.
func TestRunWizardForm_PresentsOverInjectedDriver(t *testing.T) {
	theme := tui.NewTheme(tui.Caps{})
	// Tick control 5 (prompt-injection), confirm the multiselect, then answer the
	// three injection questions — reaching two distinct screens over one driver.
	script := "5\n0\n" + "reg.example.com/detect:2\n" + "0.9\n" + "2\n"
	spy := &presentSpy{real: tui.Plain(strings.NewReader(script), io.Discard, theme)}
	pres := &Presentation{in: strings.NewReader(script), out: io.Discard, theme: theme, driver: spy}

	_, _, err := runWizardForm(context.Background(), pres, settingswizard.Selections{}, "", nil)
	require.NoError(t, err)

	want := []string{
		settingsScreens(settingswizard.Selections{}, "", nil)[0].ID(), // controls
		settingsScreens(settingswizard.Selections{}, "", nil)[1].ID(), // prompt-injection
	}
	assert.Equal(t, want, spy.seen,
		"every screen must be presented over the injected driver; an empty or short list means useDriver built its own")
}
