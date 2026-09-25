package directorycmd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/huh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// This file is white-box (package directorycmd, not directorycmd_test)
// because both tests need the unexported wizardChrome/driverForRun seam —
// wizard_test.go's black-box tests exercise RunWizard's public behavior;
// these two pin the internal mechanism that keeps a multi-phase run's rail
// from forgetting earlier phases. relsync's github/onepassword/slack kinds
// are already registered for this package's test binary by wizard_test.go's
// blank imports (one test binary is built per directory, combining this
// package's own test files with directorycmd_test's).

// TestWizardChrome_RailCoversTheFourStaticUnitsInOrder pins the rail table
// itself. Kind, Credential and Endpoint's rail steps must be exactly their
// own screens' tui.State keys (keyKind, keyCredential, keyEndpoint) — that
// identity is what lets those phases present directly over the shared
// driver with no reframing (see RunWizard's phase 2 comment). Configuration
// must be a fourth, distinct step so tui.Reframe has somewhere to pin the
// chosen kind's own screens that cannot collide with a real per-kind
// ConfigScreen key — github's is "orgs", asserted here too.
func TestWizardChrome_RailCoversTheFourStaticUnitsInOrder(t *testing.T) {
	ch := wizardChrome(tui.NewTheme(tui.Caps{}))

	assert.Equal(t, 0, ch.StepIndex(keyKind))
	assert.Equal(t, 1, ch.StepIndex(keyCredential))
	assert.Equal(t, 2, ch.StepIndex(keyEndpoint))
	assert.Equal(t, 3, ch.StepIndex(keyConfiguration))
	assert.Equal(t, -1, ch.StepIndex("orgs"),
		"a real kind's own ConfigScreen key must not collide with a rail step, or tui.Reframe's "+
			"\"configuration\" pin would be redundant with an (incidentally correct, but wrong-by-design) "+
			"direct resolution")
}

// presentSpy wraps a real Driver and records the ID of every screen actually
// presented THROUGH IT, in order. It is what lets
// TestRunWizard_BuildsOneDriverSharedAcrossPhases assert on the property
// (every phase's screens were served by the one driver driverForRun
// produced) instead of a fingerprint of it (driverForRun was merely called
// once) — the latter stays green even when a half-finished refactor resolves
// the driver once and then never threads it into Options.Driver, leaving
// every phase to resolve (and silently discard) its own.
type presentSpy struct {
	real tui.Driver
	seen []string
}

func (s *presentSpy) Present(ctx context.Context, screenID string, g *huh.Group) error {
	s.seen = append(s.seen, screenID)
	return s.real.Present(ctx, screenID, g)
}

// TestRunWizard_BuildsOneDriverSharedAcrossPhases pins the mechanism behind
// the fix: RunWizard must resolve its driver EXACTLY ONCE per run — via
// driverForRun — and every phase's screens must actually be PRESENTED over
// that same driver: directly for kind/credential/endpoint (whose IDs already
// match a wizardChrome step), and via tui.Reframe for the chosen kind's own
// config screens.
//
// This is the property whose absence caused the bug this test guards: two
// (or more) independently-resolved drivers, each framed from only its own
// phase's screens, rendered rails that had never heard of each other's
// progress — phase two's rail could not mark "Kind" done because its chrome
// never had a "Kind" step to begin with.
//
// Counting driverForRun's calls alone is NOT sufficient: a caller can resolve
// the driver once (so the count stays 1, and the chrome it was built from is
// still the full four-step one) and then simply never assign the result to
// Options.Driver anywhere — every phase then resolves (and discards) its own,
// the original bug exactly, and a test that only counted calls would stay
// green through it. presentSpy closes that gap: it wraps whatever driver
// driverForRun returns, and this test asserts every phase's screen IDs were
// recorded on that ONE spy, in order — a half-finished refactor that drops
// the Options.Driver wiring leaves spy.seen empty (every phase resolves a
// DIFFERENT, un-spied driver instead), which this test tells apart from the
// fix.
//
// This can only be observed by actually presenting groups, which the
// self-seeding NonInteractive path this package's other tests use
// deliberately avoids (see credentialScreen/endpointScreen/
// configFieldScreen's Prepare) — under NonInteractive with every answer
// pre-seeded, no screen ever calls Driver.Present at all, so a driver could
// be wired backwards and every one of those tests would stay green. This
// test instead scripts a real (off-TTY) run over the line-oriented driver,
// answering each screen with typed input, so every phase's Present call
// actually happens and can be attributed to a driver instance.
//
// A rendered-RAIL assertion (the actual visible symptom: which step lights
// up) is still not reachable from this package's tests — the line-oriented
// driver this test necessarily uses discards chrome/screenID entirely (see
// pkg/cli/tui/driver_plain.go's own doc, "the driver used off-TTY ... by
// every test"), and the one driver that draws a rail (ttyDriver) is only
// exercisable through a real or substituted bubbletea program, an
// unexported seam of pkg/cli/tui a caller in this package cannot reach.
// Attributing every phase's Present calls to one driver instance is the
// strongest property available short of that: pkg/cli/tui's own tests
// (chrome_test.go, driver_tty_test.go) already establish that ONE driver
// carrying ONE chrome renders a continuous rail; what this package needs to
// prove for itself is that RunWizard actually hands every phase that same
// one, which is exactly what spy.seen pins.
func TestRunWizard_BuildsOneDriverSharedAcrossPhases(t *testing.T) {
	prev := driverForRun
	t.Cleanup(func() { driverForRun = prev })

	var built []*presentSpy
	driverForRun = func(p tui.DriverParams) tui.Driver {
		spy := &presentSpy{real: tui.DriverFor(p)}
		built = append(built, spy)
		return spy
	}

	deps := Deps{
		Existing: func(context.Context, string) (relsync.ExistingConfig, error) {
			return relsync.ExistingConfig{}, nil
		},
		Credentials: func(context.Context) ([]CredentialRef, error) {
			// Exactly one, so the credential screen's Select has exactly one
			// option and the script can answer it with a bare "1" regardless
			// of option ordering.
			return []CredentialRef{{Identity: "ghid", Credential: "pat"}}, nil
		},
	}

	// The kind screen's options are 1 ("none") plus relsync.All() in order;
	// computed rather than hardcoded so a future kind registered ahead of
	// "github" in that sorted order doesn't silently misdirect this script
	// at the wrong option.
	kindIdx := -1
	for i, k := range relsync.All() {
		if k.Name() == "github" {
			kindIdx = i + 2
			break
		}
	}
	require.NotEqual(t, -1, kindIdx,
		"github must be registered — see wizard_test.go's blank import, shared by this test binary")

	// kind=github, credential=(the only option), endpoint=<blank, keeps the
	// vendor default>, orgs=acme. "github" reaches all four phases (it has a
	// real ConfigScreens entry, "orgs"), so this exercises the driver-sharing
	// property across the dynamic phase too, not only the three static ones.
	script := fmt.Sprintf("%d\n1\n\nacme\n", kindIdx)
	sel, err := RunWizard(context.Background(), deps, WizardOpts{
		Pres: &Presentation{In: strings.NewReader(script), Out: io.Discard, Theme: tui.NewTheme(tui.Caps{})},
	})

	// Checked before require.NoError below: driverForRun's own call happens
	// unconditionally near the top of RunWizard, before any phase runs, so
	// this holds regardless of whether the run went on to succeed — and it
	// is the assertion this test exists for, so it must not be skipped by an
	// early return from a failed require.NoError.
	require.Len(t, built, 1, "driverForRun must be called exactly once per run")
	spy := built[0]
	assert.Equal(t, []string{keyKind, keyCredential, keyEndpoint, "orgs"}, spy.seen,
		"every phase's screens must be presented over the ONE driver driverForRun returned — "+
			"a phase whose screens are missing here resolved (and silently discarded) a driver of its own")

	require.NoError(t, err)
	require.NotNil(t, sel)
	assert.Equal(t, "github", sel.Kind)
	assert.Equal(t, "ghid", sel.Identity)
	assert.Equal(t, "pat", sel.Credential)
	assert.Empty(t, sel.Endpoint, "a blank Enter keeps github's endpoint at its vendor default")
	assert.JSONEq(t, `{"orgs":["acme"]}`, string(sel.Config))
}
