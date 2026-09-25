package directorycmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/github_pat"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/onepassword_scim"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/slack_bot_token"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
	"github.com/authzed/openagentprimitives/pkg/x/browser/browsertest"
)

// This file is white-box (package directorycmd) because it reaches
// setupFlowFor, setupNewCredential and runCredentialSetup — the mechanism that
// turns "I have no credential" from a dead end into a flow. wizard_test.go's
// black-box tests cover RunWizard's public answers; these cover the seam.
//
// relsync's github/onepassword/slack kinds are registered for this test binary
// by wizard_test.go's blank imports (one test binary is built per directory).

// setupFixture is what a scripted setup run records: every page a flow asked
// to open, and every credential that reached the store.
type setupFixture struct {
	opened []string
	stored []storedCredential
}

// storedCredential is one call to Deps.StoreCredential, flattened so an
// assertion can name the token without reaching through builtins.StoreValue.
type storedCredential struct {
	cred  NewCredential
	token string
}

// registerSetupFlows installs, for this test, the real credential-setup flow
// every shipped relsync kind declares — each recording the page it would open
// instead of launching one.
//
// Two properties come out of doing it HERE rather than blank-importing the
// flow loader the way `oap`'s own main does.
//
// First, no test in this package can open a browser on the machine running the
// suite: the loader is not linked into this test binary, so the flow registry
// starts EMPTY, and a test that forgot this helper gets runCredentialSetup's
// loud "this build has not registered" refusal rather than a window on
// somebody's desktop. The unsafe path is not merely discouraged, it is absent.
//
// Second, registering each flow UNDER THE NAME ITS KIND DECLARED is itself the
// assertion that the two agree: a kind whose SetupFlow() named something no
// shipped flow answers to would fail every test below, by name.
func registerSetupFlows(t *testing.T, fx *setupFixture) {
	t.Helper()
	record := func(u string) error {
		fx.opened = append(fx.opened, u)
		return nil
	}
	builtins.Reset()
	t.Cleanup(builtins.Reset)
	// The empty directory is the same kind of injection the recording opener
	// is: slack-bot-token writes the app manifest it generates beside the run,
	// and empty means this one has nowhere to write. No test in this package is
	// about that file, and none of them leaves one in this package's directory.
	builtins.Register(slack_bot_token.New(record, ""))
	builtins.Register(onepassword_scim.New(record))

	// github-pat takes no opener: it calls browser.Open, which declines to
	// reach the OS from a test binary on its own. The recorder is installed so
	// this helper still sees the URL that flow would have opened, alongside the
	// two above that are handed `record` directly.
	browsertest.Use(t, record)
	builtins.Register(github_pat.New())

	// The second thing a setup run reaches for is the network: the wizard
	// live-verifies a credential before storing it, and github-pat's provider
	// declares a real probe against api.github.com. Left alone, this test
	// would send a made-up token to GitHub and assert on whatever came back —
	// a unit test that needs the internet, fails on a plane, and rate-limits
	// CI. Stubbed here for the same reason the browser is: in the one helper
	// every run goes through, so it is not something to remember.
	stubVerification(t, http.StatusOK, `{"login":"demo-bot","id":4242}`)
}

// stubVerification answers every live-verification probe with one canned
// response, for the duration of t.
func stubVerification(t *testing.T, status int, body string) {
	t.Helper()
	builtins.SetVerifyHTTPClient(func() *http.Client {
		return &http.Client{Transport: cannedTransport{status: status, body: body}}
	})
	t.Cleanup(func() { builtins.SetVerifyHTTPClient(nil) })
}

// cannedTransport answers any request with a fixed status and body.
type cannedTransport struct {
	status int
	body   string
}

func (c cannedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: c.status,
		Body:       io.NopCloser(strings.NewReader(c.body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

// setupDeps builds the Deps a scripted setup run needs: no credentials in the
// namespace at all, and a store that records instead of writing to a cluster.
func setupDeps(fx *setupFixture, existing []CredentialRef) Deps {
	return Deps{
		Existing: func(context.Context, string) (relsync.ExistingConfig, error) {
			return relsync.ExistingConfig{}, nil
		},
		Credentials: func(context.Context) ([]CredentialRef, error) {
			return existing, nil
		},
		StoreCredential: func(_ context.Context, cred NewCredential, v builtins.StoreValue) error {
			fx.stored = append(fx.stored, storedCredential{cred: cred, token: v.Bearer})
			return nil
		},
	}
}

// kindOption is the number to type at the kind screen to select kind.
//
// Computed rather than hardcoded, the same way wizard_chrome_test.go does it,
// so a future kind registered ahead of this one in relsync.All()'s order does
// not silently misdirect a script at the wrong option. Option 1 is "none".
func kindOption(t *testing.T, kind string) int {
	t.Helper()
	for i, k := range relsync.All() {
		if k.Name() == kind {
			return i + 2
		}
	}
	t.Fatalf("relsync kind %q is not registered; see wizard_test.go's blank imports", kind)
	return 0
}

// scriptedRun drives RunWizard over a typed script, exactly as a person at a
// terminal would answer it.
//
// It cannot use the seeded non-interactive path this package's other wizard
// tests use, and that is the point: the setup phase exists only for a run with
// a human in it, so proving it is reached at all requires actually presenting
// its screens.
func scriptedRun(t *testing.T, deps Deps, script string) (*Selections, error) {
	t.Helper()
	_, sel, err := runScript(t, deps, script, io.Discard)
	return sel, err
}

// capturedRun is scriptedRun with the terminal kept, for the assertions that
// are about what the operator READ rather than about what was stored. Guidance
// is the whole product on those screens: it is what an operator acts on before
// they have anything to test against.
func capturedRun(t *testing.T, deps Deps, script string) (string, error) {
	t.Helper()
	var out strings.Builder
	seen, _, err := runScript(t, deps, script, &out)
	return seen, err
}

func runScript(t *testing.T, deps Deps, script string, out io.Writer) (string, *Selections, error) {
	t.Helper()
	buf, capture := out.(*strings.Builder)
	sel, err := RunWizard(context.Background(), deps, WizardOpts{
		Pres: &Presentation{
			In:    strings.NewReader(script),
			Out:   out,
			Theme: tui.NewTheme(tui.Caps{}),
		},
	})
	if capture {
		return buf.String(), sel, err
	}
	return "", sel, err
}

// TestRunWizard_WithNoCredentialsReachesTheSetupFlow is the behavior this
// whole change exists for, and the one to break first when checking that these
// tests bite.
//
// Before: AvailableCredentials returning nothing left the credential select
// with zero options, so the answer stayed empty and Apply refused with "a
// credential is required" — the operator was told what they needed and given
// no way to make it. After: the setup option is the ONLY option, choosing it
// runs the kind's declared flow inline, and the credential that flow mints is
// what the rest of the run configures the source with.
//
// Asserted on what was STORED and on the resulting Selections, never on the
// absence of an error: a scripted run whose input runs out completes with a
// nil error and nothing written, so "no error" would pass for a wizard that
// reached the dead end and kept quiet about it.
func TestRunWizard_WithNoCredentialsReachesTheSetupFlow(t *testing.T) {
	const token = "xoxb-1234567890123-1234567890123-abcdefghijklmnop"
	fx := &setupFixture{}
	registerSetupFlows(t, fx)

	// kind=slack; credential=(the setup option, the only one offered);
	// identity=(blank, keeping the derived default); app=(option 2, an app the
	// operator already has — this run is about the wizard reaching the flow,
	// not about generating a manifest); token=<pasted>; endpoint=(blank,
	// Slack's host is fixed).
	script := fmt.Sprintf("%d\n1\n\n2\n%s\n\n", kindOption(t, "slack"), token)
	sel, err := scriptedRun(t, setupDeps(fx, nil), script)
	require.NoError(t, err)
	require.NotNil(t, sel)

	require.Len(t, fx.stored, 1, "the pasted token must have been stored exactly once")
	assert.Equal(t, token, fx.stored[0].token)
	assert.Equal(t, "slack-directory", fx.stored[0].cred.Identity,
		"the AgentIdentity is derived from the kind and created when absent")
	assert.Equal(t, "slack-bot-token", fx.stored[0].cred.Name,
		"the credential is named for the flow that minted it, so a re-run updates it rather than adding a second")

	assert.Equal(t, "slack-directory", sel.Identity,
		"the source must be configured with the credential the run just created")
	assert.Equal(t, "slack-bot-token", sel.Credential)
	assert.Equal(t, []string{"https://api.slack.com/apps"}, fx.opened,
		"the flow's own browser step must have run, over this wizard's driver")
}

// TestRunWizard_OffersSetupEvenWhenCredentialsExist guards the narrower dead
// end: an operator who already has a credential for some OTHER kind routinely
// needs a fresh one for this kind, and offering the option only to an empty
// namespace would make "delete everything first" the way to reach it.
func TestRunWizard_OffersSetupEvenWhenCredentialsExist(t *testing.T) {
	const token = "xoxb-1234567890123-1234567890123-abcdefghijklmnop"
	fx := &setupFixture{}
	registerSetupFlows(t, fx)

	// Two existing credentials, so the setup option is the THIRD row — which
	// is also what proves it is offered last rather than in place of them.
	existing := []CredentialRef{
		{Identity: "ghid", Credential: "pat"},
		{Identity: "opid", Credential: "tok"},
	}
	// The 2 after the blank identity is the flow's own app question, answered
	// "one I already have".
	script := fmt.Sprintf("%d\n3\n\n2\n%s\n\n", kindOption(t, "slack"), token)
	sel, err := scriptedRun(t, setupDeps(fx, existing), script)
	require.NoError(t, err)
	require.NotNil(t, sel)

	require.Len(t, fx.stored, 1)
	assert.Equal(t, "slack-directory/slack-bot-token", sel.Identity+"/"+sel.Credential,
		"picking the setup row must configure the source with the new credential, not with one of the existing rows")
}

// TestRunWizard_SetupRunsForEveryKindThatDeclaresAFlow is the kind-agnostic
// claim: the wizard never branches on a kind's name, so every kind that
// declares a flow must be drivable through the same path with nothing added
// here but that kind's own answers.
//
// It is also where the Request SHAPE is proven. runCredentialSetup hands a
// flow only Provider, UserIntent, Namespace, IdentityName and Store — Kind,
// Target, Requirement and K8s are left unset, on the argument that no flow a
// kind can name reads them. That argument is checked here rather than
// asserted: a flow that did read one would describe no screens, or store
// nothing, against this deliberately minimal request.
func TestRunWizard_SetupRunsForEveryKindThatDeclaresAFlow(t *testing.T) {
	cases := []struct {
		name string
		kind string
		// preToken is what the flow asks BEFORE the token, scripted verbatim.
		// slack-bot-token opens by asking where the app comes from ("2" — one
		// the operator already has, so this case stays about the token); the
		// other two flows ask nothing first.
		preToken   string
		token      string
		endpoint   string
		config     string
		wantIdent  string
		wantCred   string
		wantOpened string
	}{
		{
			name: "slack: a bot token, and the app question its flow opens with",
			kind: "slack", preToken: "2\n", token: "xoxb-1234567890123-1234567890123-abcdefghij",
			endpoint: "", wantIdent: "slack-directory", wantCred: "slack-bot-token",
			wantOpened: "https://api.slack.com/apps",
		},
		{
			name: "onepassword: a bridge token, plus the endpoint the kind requires",
			kind: "onepassword", token: "op-scim-token-value",
			endpoint: "https://scim.demo.invalid", wantIdent: "onepassword-directory",
			wantCred: "onepassword-scim", wantOpened: "https://support.1password.com/scim/",
		},
		{
			name: "github: a PAT, plus the org list github's own ConfigScreens asks for",
			kind: "github", token: "ghp_abcd1234efgh5678",
			endpoint: "", config: "demo-org", wantIdent: "github-directory",
			wantCred: "github-pat", wantOpened: "https://github.com/settings/personal-access-tokens",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := &setupFixture{}
			registerSetupFlows(t, fx)

			script := fmt.Sprintf("%d\n1\n\n%s%s\n%s\n",
				kindOption(t, tc.kind), tc.preToken, tc.token, tc.endpoint)
			if tc.config != "" {
				script += tc.config + "\n"
			}
			sel, err := scriptedRun(t, setupDeps(fx, nil), script)
			require.NoError(t, err)
			require.NotNil(t, sel)

			require.Len(t, fx.stored, 1, "the flow must have stored exactly one credential")
			assert.Equal(t, tc.token, fx.stored[0].token)
			assert.Equal(t, tc.wantIdent, sel.Identity)
			assert.Equal(t, tc.wantCred, sel.Credential)
			require.Len(t, fx.opened, 1, "the flow's browser step must have run")
			assert.Contains(t, fx.opened[0], tc.wantOpened)
		})
	}
}

// TestEveryRegisteredKindDeclaresAResolvableFlow is the registry-level check,
// and it is what keeps the table above from quietly covering fewer kinds than
// ship. A kind added tomorrow is covered the day it registers.
//
// The provider half is not decoration. A flow with no catalog entry runs
// perfectly happily — every flow tolerates a nil provider — while silently
// losing BOTH gates on a pasted value: the declared token format, and the live
// probe. runCredentialSetup refuses that rather than storing whatever was
// typed, so a kind naming an uncatalogued flow would fail at the prompt for a
// reason nobody could act on. Better to fail here.
func TestEveryRegisteredKindDeclaresAResolvableFlow(t *testing.T) {
	registerSetupFlows(t, &setupFixture{})

	kinds := relsync.All()
	require.NotEmpty(t, kinds, "no relsync kind is registered; this test would pass vacuously")
	for _, k := range kinds {
		t.Run(k.Name(), func(t *testing.T) {
			flow, intent, _, err := setupFlowFor(k)
			require.NoError(t, err)
			require.NotEmpty(t, flow,
				"kind %q declares no credential setup flow, so an operator with no credential for it "+
					"is back at the dead end this change removed", k.Name())
			_, ok := builtins.Get(flow)
			assert.True(t, ok, "kind %q names flow %q, which no shipped flow answers to", k.Name(), flow)
			_, hasProvider := provider.ByBuiltin(flow)
			assert.Truef(t, hasProvider,
				"flow %q (named by kind %q) has no provider in the catalog, so a pasted credential "+
					"would be gated by no declared token format and verified against nothing",
				flow, k.Name())
			assert.NotEmpty(t, intent,
				"an empty intent leaves a flow with nothing to narrow its guidance from")
		})
	}
}

// TestRunWizard_RefusesToOverwriteAnExistingCredential is the fix for a silent
// replacement, and the scenario is ordinary rather than contrived.
//
// The credential's name is DERIVED from the flow, so an AgentIdentity that
// already carries a "github-pat" — an entirely idiomatic name for one — is a
// name collision waiting for the first operator who types that identity to
// keep things tidy. setup.Store updates the Secret value of a credential it
// finds under that name, so the directory sync's org-read token would replace
// the repo-write token some toolkit depends on, and that toolkit would start
// failing with nothing in this command's output mentioning a replacement.
func TestRunWizard_RefusesToOverwriteAnExistingCredential(t *testing.T) {
	fx := &setupFixture{}
	registerSetupFlows(t, fx)

	// The identity the operator is about to name already holds a credential
	// under exactly the name this run would write.
	existing := []CredentialRef{{Identity: "slack-directory", Credential: "slack-bot-token"}}

	// kind=slack; credential=(the setup option, offered second); identity=
	// (blank, keeping the default — which is the colliding identity).
	script := fmt.Sprintf("%d\n2\n\n", kindOption(t, "slack"))
	sel, err := scriptedRun(t, setupDeps(fx, existing), script)
	require.Error(t, err)
	assert.Nil(t, sel)
	assert.Contains(t, err.Error(), "already has a credential named",
		"the refusal must name the conflict, not merely decline")
	assert.Contains(t, err.Error(), "oap identity put-token",
		"and name the command that updates one in place, since that is what the operator may have meant")
	assert.Contains(t, err.Error(), "--credential slack-bot-token",
		"the suggested command must fill in --credential, which put-token requires, or pasting it fails "+
			"with a usage error instead of running")
	assert.Contains(t, err.Error(), "--from-stdin",
		"the suggested command must carry a --from-* source, which put-token also requires, so it is "+
			"copy-pasteable rather than one more thing the operator has to figure out")
	assert.Empty(t, fx.stored, "nothing may be written over a credential something else depends on")
}

// TestRunWizard_IdentityGuidanceMatchesWhatTheRunDoes closes the gap the
// refusal above was hiding behind: the screen used to promise that an existing
// identity was "reused … nothing already on it is replaced", which was true of
// the identity and false of the credential on it. A refusal the guidance does
// not predict is still a wasted browser trip and a token already pasted.
func TestRunWizard_IdentityGuidanceMatchesWhatTheRunDoes(t *testing.T) {
	fx := &setupFixture{}
	registerSetupFlows(t, fx)

	const token = "xoxb-1234567890123-1234567890123-abcdefghijklmnop"
	script := fmt.Sprintf("%d\n1\n\n2\n%s\n\n", kindOption(t, "slack"), token)
	out, err := capturedRun(t, setupDeps(fx, nil), script)
	require.NoError(t, err)

	assert.Contains(t, out, "slack-bot-token",
		"the screen must name the credential it is about to create")
	assert.Contains(t, out, "refused, not overwritten",
		"and say what happens when the identity already has one by that name")
	assert.NotContains(t, out, "nothing already on it is replaced",
		"the old promise was false for the credential and is what made the overwrite silent")
}

// TestRunWizard_OffersToGenerateTheSlackApp is the wizard-side half of the
// generated-manifest route: the flow can render an app manifest, and this
// proves `oap directory configure` actually PUTS it in front of the operator,
// with the scopes the sync calls with, rather than sending them to
// api.slack.com with a list to transcribe.
//
// users:read.email is the assertion that matters. An app created from a
// manifest missing it installs cleanly, syncs, and writes nothing — Slack
// answers users.info without the email field rather than refusing the call —
// so a generated app short of it would be worse than the manual scope list it
// replaces.
func TestRunWizard_OffersToGenerateTheSlackApp(t *testing.T) {
	fx := &setupFixture{}
	registerSetupFlows(t, fx)

	// …identity=(blank); app=1, the generate-it-for-me route; token=<pasted>.
	const token = "xoxb-1234567890123-1234567890123-abcdefghijklmnop"
	script := fmt.Sprintf("%d\n1\n\n1\n%s\n\n", kindOption(t, "slack"), token)
	out, err := capturedRun(t, setupDeps(fx, nil), script)
	require.NoError(t, err)

	require.Len(t, fx.stored, 1, "the run must still end with the pasted token stored")
	assert.Equal(t, token, fx.stored[0].token)

	assert.Contains(t, out, "display_information:",
		"the app manifest itself must reach the operator; it is what they paste into Slack")
	for _, scope := range []string{"channels:read", "groups:read", "users:read", "users:read.email"} {
		assert.Containsf(t, out, "- "+scope, "the generated app must request %s", scope)
	}
}

// TestRunWizard_PrintsTheKindsOwnScopeRecommendation is the wizard-side half of
// the github scope fix. The kind declares the fine-grained permissions its
// calls need (and a test in its own package pins those against the calls);
// this proves the wizard actually PUTS them in front of the operator, in place
// of github-pat's own repository-permission guess.
//
// Without it the failure is invisible until a cluster 403s: the flow's probe is
// GET /user, which any token answers, so a token granted exactly the wrong
// access verifies clean and stores clean.
func TestRunWizard_PrintsTheKindsOwnScopeRecommendation(t *testing.T) {
	fx := &setupFixture{}
	registerSetupFlows(t, fx)

	script := fmt.Sprintf("%d\n1\n\nghp_abcd1234efgh5678\n\ndemo-org\n", kindOption(t, "github"))
	out, err := capturedRun(t, setupDeps(fx, nil), script)
	require.NoError(t, err)

	for _, want := range []string{"Organization permissions", "Members: read", "Resource owner"} {
		assert.Containsf(t, out, want,
			"the guidance must name %q — it is the permission this sync's org reads actually need", want)
	}
	assert.NotContains(t, out, "pull_requests: read",
		"github-pat's own repository-permission guess must not be what the operator is sent to request")
}

// TestSetupFlowFor_RefusesAKindThatDeclaresNothing pins the fail-closed half
// of the declaration. Treating an empty flow name as "declares none" would
// degrade silently to the old dead end for the one kind whose author had
// clearly meant to avoid it.
func TestSetupFlowFor_RefusesAKindThatDeclaresNothing(t *testing.T) {
	_, _, _, err := setupFlowFor(brokenSetupKind{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "names no flow")
}

// brokenSetupKind implements relsync.CredentialSetup and then names nothing.
// It is never registered with relsync — setupFlowFor takes the interface, so
// the contract can be exercised without adding a kind to the process-wide
// registry every other test in this binary reads.
type brokenSetupKind struct{ relsync.Kind }

func (brokenSetupKind) Name() string                     { return "broken" }
func (brokenSetupKind) SetupFlow() (flow, intent string) { return "  ", "an intent with no flow" }
func (brokenSetupKind) SetupScopes() []string            { return nil }

// nonInteractiveRun is the --non-interactive counterpart of scriptedRun: no
// terminal at all, answers supplied up front, fail closed at the first screen
// that was not answered.
func nonInteractiveRun(t *testing.T, deps Deps, answers map[string]string) (*Selections, error) {
	t.Helper()
	return RunWizard(context.Background(), deps, WizardOpts{
		Answers:        answers,
		NonInteractive: true,
	})
}

// TestRunWizard_NonInteractiveStillRequiresAnExistingCredential is the other
// half of the contract, and the half a reader is most likely to assume got
// loosened: the setup flow is reachable ONLY with a human present, because
// obtaining a credential means pasting a secret, and a secret supplied as a
// flag lands in shell history, in the process table, and in any answer file
// the command is replayed from.
//
// Every case asserts on what the refusal SAYS, not merely that it refused. The
// message this replaces — a bare "a credential is required" — was true and
// useless: it named a requirement and neither of the two ways to meet it, and
// the one this run cannot take is exactly the one the operator needs told.
func TestRunWizard_NonInteractiveStillRequiresAnExistingCredential(t *testing.T) {
	fx := &setupFixture{}
	registerSetupFlows(t, fx)

	cases := []struct {
		name     string
		existing []CredentialRef
		answers  map[string]string
		contains []string
	}{
		{
			name:     "no credentials at all: refused, naming both ways out",
			existing: nil,
			answers:  map[string]string{keyKind: "slack"},
			contains: []string{
				"--non-interactive requires a credential that already exists",
				"Re-run without --non-interactive",
				"oap identity setup",
			},
		},
		{
			name:     "credentials exist but none was named: refused, naming the flag that would",
			existing: []CredentialRef{{Identity: "ghid", Credential: "pat"}},
			answers:  map[string]string{keyKind: "slack"},
			contains: []string{
				"--non-interactive cannot set one up",
				"--answer credential=",
			},
		},
		{
			name:     "the setup option named through an answer: refused rather than attempted",
			existing: []CredentialRef{{Identity: "ghid", Credential: "pat"}},
			answers:  map[string]string{keyKind: "slack", keyCredential: setupNewCredential},
			contains: []string{"--non-interactive cannot set up a new credential"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sel, err := nonInteractiveRun(t, setupDeps(fx, tc.existing), tc.answers)
			require.Error(t, err)
			assert.Nil(t, sel)
			for _, want := range tc.contains {
				assert.Contains(t, err.Error(), want)
			}
			assert.Empty(t, fx.stored, "a run told not to prompt must never mint a credential")
			assert.Empty(t, fx.opened, "a run told not to prompt must never open a browser at nobody")
		})
	}
}

// TestRunWizard_RefusesASecretSuppliedThroughAnAnswer is the loud half of the
// no-secrets-in-flags rule. The quiet half is structural — the setup phase
// runs on a State of its own, so a seeded token is inert and cannot reach the
// flow — but inert is not enough: an operator who typed one believes it was
// used, and has in the meantime put a credential somewhere that ignoring it
// does nothing to retrieve.
func TestRunWizard_RefusesASecretSuppliedThroughAnAnswer(t *testing.T) {
	fx := &setupFixture{}
	registerSetupFlows(t, fx)

	// An interactive run — so the setup phase is genuinely reached — carrying
	// the flow's own answer key as a pre-supplied answer.
	_, err := RunWizard(context.Background(), setupDeps(fx, nil), WizardOpts{
		Pres: &Presentation{
			In:    strings.NewReader(fmt.Sprintf("%d\n1\n\n", kindOption(t, "slack"))),
			Out:   io.Discard,
			Theme: tui.NewTheme(tui.Caps{}),
		},
		Answers: map[string]string{slack_bot_token.KeyToken: "xoxb-should-never-be-used"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), slack_bot_token.KeyToken,
		"the refusal must name the answer it is refusing")
	assert.Contains(t, err.Error(), "shell history",
		"and say why, because the reason is what stops the operator retrying with the same value")
	assert.Empty(t, fx.stored, "a token supplied by flag must never be stored")
}

// TestRunWizard_RefusesACredentialTheProviderRejected is the fail-closed half
// of live verification. A 401 is the provider's auth layer saying this
// credential is dead; storing it anyway buys a RelationshipSource that reports
// Ready=False forever, and the operator finds out from a condition nobody
// watches rather than at the prompt where they could paste another.
func TestRunWizard_RefusesACredentialTheProviderRejected(t *testing.T) {
	fx := &setupFixture{}
	registerSetupFlows(t, fx)
	stubVerification(t, http.StatusUnauthorized, `{"message":"Bad credentials"}`)

	script := fmt.Sprintf("%d\n1\n\nghp_abcd1234efgh5678\n\ndemo-org\n", kindOption(t, "github"))
	sel, err := scriptedRun(t, setupDeps(fx, nil), script)
	require.Error(t, err)
	assert.Nil(t, sel)
	assert.Contains(t, err.Error(), "rejected")
	assert.Contains(t, err.Error(), "Nothing was stored")
	assert.Empty(t, fx.stored, "a credential the provider refused must not be written")
}

// TestRunWizard_RecordsWhatVerificationLearned pins the attestation plumbing:
// setup.Store records which provider account a credential authenticated as,
// and it can only do that if the verdict's own ProviderID and SubjectID reach
// it. Dropping either is silent — the credential still works — so nothing but
// an assertion here would notice.
func TestRunWizard_RecordsWhatVerificationLearned(t *testing.T) {
	fx := &setupFixture{}
	registerSetupFlows(t, fx) // the canned 200 answers as login demo-bot, id 4242

	script := fmt.Sprintf("%d\n1\n\nghp_abcd1234efgh5678\n\ndemo-org\n", kindOption(t, "github"))
	_, err := scriptedRun(t, setupDeps(fx, nil), script)
	require.NoError(t, err)

	require.Len(t, fx.stored, 1)
	assert.Equal(t, "github-pat", fx.stored[0].cred.ProviderID,
		"the catalog provider that ran the check must travel with the credential")
	assert.Equal(t, "4242", fx.stored[0].cred.SubjectID,
		"the provider's STABLE id is what an authorization edge is keyed on, not the mutable login")
}
