package directorycmd_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/directorycmd"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"

	// Registers the "github" and "onepassword" relsync kinds this file's
	// tests exercise. The production `oap directory` command (Task 7) is
	// where these blank imports belong for the shipped binary; this test
	// file needs its own so `relsync.All()`/`relsync.Get` see them inside
	// this package's own test binary, which does not otherwise reach either
	// package.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/onepassword"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// wizardFixture is what runWizardSeeded's opts customize: the credentials
// AvailableCredentials would have found, and the ExistingConfig ExistingFor
// would have found. Both default to values every mandated test except the
// ones that explicitly override them can rely on.
type wizardFixture struct {
	// credentials defaults to two entries — "ghid/pat" and "opid/tok" — so a
	// test that names either as its "credential" answer (or, via
	// withExisting, has either as its prior credential) validates against a
	// real available credential without needing its own opt.
	credentials []directorycmd.CredentialRef
	existing    *relsync.ExistingConfig
}

// withNoCredentials empties the fixture's credential list, exercising the
// "nothing to select" refusal.
func withNoCredentials() func(*wizardFixture) {
	return func(f *wizardFixture) { f.credentials = nil }
}

// withExisting seeds what a prior RelationshipSource already configured, so
// a re-run's "enter through" behavior can be exercised without building a
// dynamic-client fixture — ExistingFor's own fixtures already cover that
// function directly (existing_test.go).
func withExisting(cfg relsync.ExistingConfig) func(*wizardFixture) {
	return func(f *wizardFixture) { f.existing = &cfg }
}

// runWizardSeeded drives directorycmd.RunWizard non-interactively: answers
// is pre-seeded into tui.State via WizardOpts.Answers, and
// WizardOpts.NonInteractive fails the run closed at the first screen whose
// answer wasn't supplied — the mode that exists precisely so a wizard is
// testable without a terminal.
func runWizardSeeded(t *testing.T, answers map[string]string, opts ...func(*wizardFixture)) (*directorycmd.Selections, error) {
	t.Helper()
	fx := &wizardFixture{
		credentials: []directorycmd.CredentialRef{
			{Identity: "ghid", Credential: "pat"},
			{Identity: "opid", Credential: "tok"},
		},
	}
	for _, o := range opts {
		o(fx)
	}

	deps := directorycmd.Deps{
		Existing: func(context.Context, string) (relsync.ExistingConfig, error) {
			if fx.existing != nil {
				return *fx.existing, nil
			}
			return relsync.ExistingConfig{}, nil
		},
		Credentials: func(context.Context) ([]directorycmd.CredentialRef, error) {
			return fx.credentials, nil
		},
	}

	return directorycmd.RunWizard(context.Background(), deps, directorycmd.WizardOpts{
		Answers:        answers,
		NonInteractive: true,
	})
}

// Selecting no kinds is a first-class answer, not a cancelled run.
func TestRunWizard_SelectingNoKindConfiguresNothing(t *testing.T) {
	sel, err := runWizardSeeded(t, map[string]string{"kind": ""})
	require.NoError(t, err)
	assert.Nil(t, sel, "no kind chosen means no CR, and no error")
}

// The seam is what makes this kind-agnostic: the kind's own screens are
// sequenced, and their answers become spec.config.
func TestRunWizard_AsksTheKindsOwnQuestions(t *testing.T) {
	sel, err := runWizardSeeded(t, map[string]string{
		"kind": "github", "credential": "ghid/pat", "orgs": "acme",
	})
	require.NoError(t, err)
	require.NotNil(t, sel)
	assert.Equal(t, "github", sel.Kind)
	assert.Equal(t, "ghid", sel.Identity)
	assert.Equal(t, "pat", sel.Credential)
	assert.JSONEq(t, `{"orgs":["acme"]}`, string(sel.Config))
}

// With no credential in the namespace the wizard must refuse and say what to
// do, rather than write a source that can never reconcile.
func TestRunWizard_RefusesWhenNoCredentialExists(t *testing.T) {
	_, err := runWizardSeeded(t, map[string]string{"kind": "github"},
		withNoCredentials())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "oap identity",
		"the error names the command that creates one")
}

// A re-run keeps what is configured when the operator enters through.
func TestRunWizard_EnterThroughKeepsExistingConfig(t *testing.T) {
	sel, err := runWizardSeeded(t, map[string]string{"kind": "github"},
		withExisting(relsync.ExistingConfig{
			Name: "gh", Identity: "ghid", Credential: "pat",
			Config: json.RawMessage(`{"orgs":["acme"]}`),
		}))
	require.NoError(t, err)
	require.NotNil(t, sel)
	assert.Equal(t, "gh", sel.Name, "a re-run edits the same CR, it does not create a second")
	assert.JSONEq(t, `{"orgs":["acme"]}`, string(sel.Config))
}

// A kind whose endpoint is required must not be configurable without one.
func TestRunWizard_RefusesAnEmptyEndpointForAKindThatNeedsOne(t *testing.T) {
	_, err := runWizardSeeded(t, map[string]string{
		"kind": "onepassword", "credential": "opid/tok", "endpoint": "",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "endpoint")
}
