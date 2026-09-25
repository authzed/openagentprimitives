package registry_test

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp"

	// Blank imports register the real kinds this contract is asserted over.
	// fakekind is here for the no-setup-flow case, and is excluded from the
	// setup properties by setupKinds.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/fakekind"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/googlekind"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/oidckind"
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/idp/passwordkind"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/registry"
)

// registeredKinds is the real kind set, snapshotted at init.
//
// Captured here rather than read inside each test because registry_test.go
// calls registry.Reset to install stubs, and a contract asserted over whatever
// the registry happens to hold at the time would quietly assert nothing. The
// blank imports above are what guarantee the real kinds have registered by the
// time this runs: an imported package's init completes before the importing
// package's does.
var registeredKinds = snapshotKinds()

func snapshotKinds() []idp.Kind {
	var out []idp.Kind
	for _, name := range registry.Names() {
		if k, ok := registry.Get(name); ok {
			out = append(out, k)
		}
	}
	return out
}

// setupKinds is the subset a human can actually run `oap idp setup` against.
// The fake kind registers a wizard that refuses, which is its whole contract —
// asserting the setup properties against it would assert nothing.
func setupKinds(t *testing.T) []idp.Kind {
	t.Helper()
	var out []idp.Kind
	for _, k := range registeredKinds {
		if k.Name() == "fake" {
			continue
		}
		out = append(out, k)
	}
	require.NotEmpty(t, out, "no real idp kinds registered; this contract would assert nothing")
	return out
}

// answerFor supplies one kind-appropriate answer per State key, for the seeded
// runs below. The values are deliberately recognisable so a leak into a note is
// unmistakable, and valid enough to pass each field's own validator.
//
// A table rather than one generic string because the answers are not
// interchangeable: an issuer must parse as an https URL, a password has a
// length floor, and an access policy must be one of the offered options. What
// keeps it from rotting is that every test using it REQUIRES an entry for every
// key a kind declares, and fails naming the key when one is missing — so a
// question added without an entry is reported rather than silently skipped.
func answerFor() map[string]string {
	return map[string]string{
		"issuer":        "https://login.demo-idp.example/",
		"client-id":     "CONTRACTSENTINELCLIENTID.apps.googleusercontent.com",
		"client-secret": "CONTRACTSENTINELCLIENTSECRET",
		"access":        "domains",
		"domains":       "demo-corp.example",
		"password":      "CONTRACTSENTINELPASSWORD",
		"confirm":       "CONTRACTSENTINELPASSWORD",
	}
}

// declaredKeys returns the State keys these screens say they ask for, in screen
// order, deduplicated.
func declaredKeys(t *testing.T, screens []tui.Screen) []string {
	t.Helper()
	var all []string
	seen := map[string]bool{}
	for _, s := range screens {
		keyer, ok := s.(interface{ AnswerKeys() []string })
		if !ok {
			continue
		}
		for _, k := range keyer.AnswerKeys() {
			if !seen[k] {
				seen[k] = true
				all = append(all, k)
			}
		}
	}
	return all
}

// seedAll answers every key the screens declare, so a run prompts for nothing.
func seedAll(t *testing.T, screens []tui.Screen) *tui.State {
	t.Helper()
	answers := answerFor()
	st := tui.NewState()
	for _, k := range declaredKeys(t, screens) {
		v, ok := answers[k]
		require.True(t, ok, "key %q has no entry in answerFor, so its handling is unchecked; add one", k)
		st.Set(k, v)
	}
	return st
}

// runSeeded drives a kind's wizard to completion with every question already
// answered, and returns the produced output alongside the answered State.
func runSeeded(t *testing.T, k idp.Kind, in idp.WizardInput) (idp.WizardOutput, *tui.State) {
	t.Helper()
	w := k.Wizard()
	screens, err := w.Screens(context.Background(), in)
	require.NoError(t, err, "%s must describe screens", k.Name())

	st := seedAll(t, screens)
	var out bytes.Buffer
	answered, err := tui.RunWith(context.Background(), screens, tui.Options{
		Theme: tui.NewTheme(tui.Caps{}),
		In:    strings.NewReader(""),
		Out:   &out,
	}, st)
	require.NoError(t, err, "a fully seeded run must complete without prompting")
	require.Empty(t, out.String(),
		"a fully seeded run wrote to the terminal, so a question was asked that should have been skipped")

	res, err := w.Result(answered)
	require.NoError(t, err, "Result must accept a fully answered State")
	return res, answered
}

// TestEveryWizardDescribesScreensRatherThanPrompting is the contract every idp
// wizard answers to, asserted over the whole registry rather than per kind: a
// kind added tomorrow is covered the day it registers.
func TestEveryWizardDescribesScreensRatherThanPrompting(t *testing.T) {
	for _, k := range setupKinds(t) {
		t.Run(k.Name(), func(t *testing.T) {
			w := k.Wizard()
			require.NotNil(t, w, "a kind with no wizard cannot be set up")

			screens, err := w.Screens(context.Background(), idp.WizardInput{
				CallbackURL: "https://ap.demo-cluster.example/oidc/callback/idp",
			})
			require.NoError(t, err)
			require.NotEmpty(t, screens, "Screens returned no screens and no reason")

			seen := map[string]bool{}
			for i, s := range screens {
				assert.NotEmpty(t, s.ID(), "screen %d has no ID; the step rail and every wrapped error key off it", i)
				assert.NotEmpty(t, s.Label(), "screen %q has no Label; the rail would render a blank step", s.ID())
				assert.False(t, seen[s.ID()], "screen ID %q is used twice; the rail cannot highlight either", s.ID())
				seen[s.ID()] = true
			}
			assert.NotEmpty(t, declaredKeys(t, screens),
				"no screen declares an AnswerKey, so nothing can seed or name this wizard's questions")
		})
	}
}

// TestAKindWithNoSetupFlowRefusesRatherThanRunningZeroScreens covers the kinds
// setupKinds excludes, and the reason they need a non-nil Wizard at all.
//
// A nil one would be dereferenced by `oap idp setup fake`; a zero-screen one
// would run a wizard that asks nothing and then apply whatever spec an
// unanswered State produced. Saying why it cannot run is the only third option.
func TestAKindWithNoSetupFlowRefusesRatherThanRunningZeroScreens(t *testing.T) {
	// From the snapshot, not registry.Get: registry_test.go's own tests call
	// registry.Reset, so a live lookup here finds whatever they last installed.
	var k idp.Kind
	for _, candidate := range registeredKinds {
		if candidate.Name() == "fake" {
			k = candidate
		}
	}
	require.NotNil(t, k, "the fake kind must be registered for this test to mean anything")

	w := k.Wizard()
	require.NotNil(t, w, "even a kind with no setup flow needs a wizard to refuse with")

	screens, err := w.Screens(context.Background(), idp.WizardInput{})
	require.Error(t, err, "a kind with no setup flow must say so rather than hand back nothing")
	assert.Empty(t, screens, "a wizard that refuses must not also hand back screens")

	res, err := w.Result(tui.NewState())
	require.Error(t, err, "a wizard that refuses to describe itself must refuse to produce a spec")
	assert.Empty(t, res.Spec.Kind)
	assert.Nil(t, res.SecretData)
}

// TestResultRefusesAnUnansweredState is the fail-closed half.
//
// huh's accessible renderer has no error channel — an input script that runs
// out yields a fully-defaulted form and a nil error — so "the run finished"
// proves nothing about what the user typed. Result is the one place that can
// tell an answered wizard from a silent one, and these wizards produce a
// ClusterIdentityProvider and its client secret: a Result that accepted an
// unanswered State would configure cluster login against an empty issuer with
// an empty credential, behind a CLI that reported success.
func TestResultRefusesAnUnansweredState(t *testing.T) {
	for _, k := range setupKinds(t) {
		t.Run(k.Name(), func(t *testing.T) {
			w := k.Wizard()
			_, err := w.Screens(context.Background(), idp.WizardInput{
				CallbackURL: "https://ap.demo-cluster.example/oidc/callback/idp",
			})
			require.NoError(t, err)

			_, err = w.Result(tui.NewState())
			assert.Error(t, err, "Result must refuse a State that holds no answers at all")

			assert.NotPanics(t, func() {
				_, err := w.Result(nil)
				assert.Error(t, err, "a nil State carries no answers, so Result must refuse")
			})
		})
	}
}

// TestARunNobodyAnsweredProducesNothing drives the real screens over an EMPTY
// input, which is what a truncated script, a closed pipe or a `< /dev/null`
// looks like from inside the plain driver.
//
// This is the defect class that has cost this migration the most rounds: huh's
// accessible renderer reports no read error, so every field falls back to its
// bound default and the run returns nil. A wizard whose defaults happened to be
// usable would hand back a fully-formed ClusterIdentityProvider that no human
// described — in particular one whose access policy is "any account may sign
// in", which is the one answer that must never be reachable by silence.
func TestARunNobodyAnsweredProducesNothing(t *testing.T) {
	for _, k := range setupKinds(t) {
		t.Run(k.Name()+": empty input yields a refusal, never a spec", func(t *testing.T) {
			w := k.Wizard()
			screens, err := w.Screens(context.Background(), idp.WizardInput{
				CallbackURL: "https://ap.demo-cluster.example/oidc/callback/idp",
			})
			require.NoError(t, err)

			var out bytes.Buffer
			answered, err := tui.RunWith(context.Background(), screens, tui.Options{
				Theme: tui.NewTheme(tui.Caps{}),
				In:    strings.NewReader(""),
				Out:   &out,
			}, tui.NewState())
			// The run itself may well succeed — that is the whole hazard.
			if err == nil {
				res, resErr := w.Result(answered)
				require.Error(t, resErr, "an unanswered run produced %+v instead of a refusal", res.Spec)
			}
		})

		t.Run(k.Name()+": a policy question nobody answered never lands on allow-any", func(t *testing.T) {
			w := k.Wizard()
			screens, err := w.Screens(context.Background(), idp.WizardInput{
				CallbackURL: "https://ap.demo-cluster.example/oidc/callback/idp",
			})
			require.NoError(t, err)

			keys := declaredKeys(t, screens)
			if !slices.Contains(keys, "access") {
				t.Skipf("%s asks no sign-in policy question", k.Name())
			}

			// Everything EXCEPT the policy and what follows from it is seeded,
			// so the run actually reaches that question with nothing to read.
			// Seeding nothing would not do: every kind refuses at its first
			// required field long before the policy screen, which would make
			// this assertion pass without the policy ever being decided —
			// exactly the shape of an assertion that asserts nothing.
			answers := answerFor()
			st := tui.NewState()
			for _, key := range keys {
				if key == "access" || key == "domains" {
					continue
				}
				v, ok := answers[key]
				require.True(t, ok, "key %q has no entry in answerFor; add one", key)
				st.Set(key, v)
			}

			var out bytes.Buffer
			answered, _ := tui.RunWith(context.Background(), screens, tui.Options{
				Theme: tui.NewTheme(tui.Caps{}),
				In:    strings.NewReader(""),
				Out:   &out,
			}, st)
			require.True(t, answered.Has("access"),
				"the policy question was never reached, so this test proved nothing")
			assert.NotEqual(t, "any", answered.Get("access"),
				"a policy question nobody answered selected allow-any")
		})
	}
}

// TestNoCredentialReachesTheSummary is the masking contract, asserted over the
// whole registry rather than per kind.
//
// The summary is rendered to the user's scrollback after the run, where it
// outlives the terminal the credential was typed into — and it is a plain
// io.Writer, so it is equally what a `> setup.log` redirect captures. Nothing
// in a wizard's own code prevents a screen from noting the value it collected;
// this is what keeps the next one honest.
//
// The oracle is derived rather than declared: whatever the wizard turned into
// Secret material IS the credential, by definition. Both shapes count — the
// value written verbatim (oidc and google write the client secret straight
// through) and the value the Secret was DERIVED from (password writes a bcrypt
// hash, so the raw password never appears in SecretData and a verbatim-only
// check would miss it entirely).
func TestNoCredentialReachesTheSummary(t *testing.T) {
	answers := answerFor()

	for _, k := range setupKinds(t) {
		t.Run(k.Name(), func(t *testing.T) {
			res, answered := runSeeded(t, k, idp.WizardInput{
				CallbackURL: "https://ap.demo-cluster.example/oidc/callback/idp",
			})

			creds := credentialsIn(t, res, answers)
			require.NotEmpty(t, creds,
				"the wizard produced nothing recognisable as a credential, so this test proved nothing")

			notes := answered.Notes()
			require.NotEmpty(t, notes, "the wizard recorded no summary at all, so this test proved nothing")

			for _, n := range notes {
				for _, cred := range creds {
					assert.NotContains(t, n.Value, cred,
						"summary line %q carries the credential verbatim", n.Label)
					assert.NotContains(t, n.Label, cred,
						"summary label %q carries the credential verbatim", n.Label)
				}
			}
		})
	}
}

// credentialsIn returns the seeded answers this run turned into Secret
// material: the ones written through verbatim, and the ones a written hash
// verifies against.
func credentialsIn(t *testing.T, res idp.WizardOutput, answers map[string]string) []string {
	t.Helper()
	var creds []string
	for _, raw := range res.SecretData {
		stored := string(raw)
		for _, answer := range answers {
			switch {
			case stored == answer:
				creds = append(creds, answer)
			case bcrypt.CompareHashAndPassword(raw, []byte(answer)) == nil:
				creds = append(creds, answer)
			}
		}
	}
	return creds
}

// TestSeededRunProducesAUsableSpec pins the shape the CLI dispatcher depends
// on, over the whole registry: a namespace it fills in itself, and a secret ref
// that names the Secret the wizard also handed back.
func TestSeededRunProducesAUsableSpec(t *testing.T) {
	for _, k := range setupKinds(t) {
		t.Run(k.Name(), func(t *testing.T) {
			res, _ := runSeeded(t, k, idp.WizardInput{
				CallbackURL: "https://ap.demo-cluster.example/oidc/callback/idp",
			})

			assert.Equal(t, k.Name(), res.Spec.Kind, "the spec must name the kind that produced it")
			assert.NotEmpty(t, res.SecretName, "a wizard that writes Secret material must name the Secret")
			assert.Equal(t, res.SecretName, res.Spec.ClientSecretRef.Name,
				"the CR must point at the Secret the wizard handed back")
			assert.NotEmpty(t, res.Spec.ClientSecretRef.Key)
			assert.Empty(t, res.Spec.ClientSecretRef.Namespace,
				"the wizard must leave Namespace empty for the dispatcher to fill")
			require.NotNil(t, res.SecretData)
			assert.Contains(t, res.SecretData, res.Spec.ClientSecretRef.Key,
				"the written data key must match the ref the CR declares")

			// Fail-closed on the access policy: a spec that neither lists domains
			// nor explicitly allows any is refused by the CR's own webhook, and a
			// spec that does BOTH is ambiguous about which one wins.
			var spec spiceboxv1alpha1.ClusterIdentityProviderSpec = res.Spec
			assert.True(t, spec.AllowAnyEmail || len(spec.AllowedEmailDomains) > 0,
				"the spec declares no sign-in policy at all")
		})
	}
}
