package slack

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/appprovision"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
)

// fakeProvisionClient records what it was asked for and answers as configured.
type fakeProvisionClient struct {
	createErr  error
	installErr error

	// omitInstallAppID answers an install with no app_id, which is the shape
	// apps.developerInstall could take without warning: it is undocumented, so
	// its response is not a contract.
	omitInstallAppID bool

	// createCalled and installCalled are what a test proves a SKIP with: a
	// run whose provisioning step should never do any work has nothing else
	// to check that work against — the fields below all stay at their zero
	// value whether Create ran and returned nothing, or never ran at all.
	createCalled  bool
	installCalled bool

	// gotToken is the configuration token the run authenticated with. It is
	// what proves WHICH source produced it, since a source that mints its own
	// token leaves no other trace.
	gotToken        string
	gotManifestJSON string
	gotBotScopes    []string
	gotAppID        string
}

func (f *fakeProvisionClient) Create(_ context.Context, token, manifestJSON string) (appprovision.CreateResult, error) {
	f.createCalled = true
	f.gotToken, f.gotManifestJSON = token, manifestJSON
	if f.createErr != nil {
		return appprovision.CreateResult{}, f.createErr
	}
	return appprovision.CreateResult{AppID: "A0DEMO"}, nil
}

func (f *fakeProvisionClient) Install(_ context.Context, _, appID string, botScopes []string) (appprovision.InstallResult, error) {
	f.installCalled = true
	f.gotAppID, f.gotBotScopes = appID, botScopes
	if f.installErr != nil {
		return appprovision.InstallResult{}, f.installErr
	}
	res := appprovision.InstallResult{
		AppID:         appID,
		BotToken:      validBotToken,
		AppLevelToken: validAppToken,
	}
	if f.omitInstallAppID {
		res.AppID = ""
	}
	return res, nil
}

// stubSlackCLIOnPATH puts a fake `slack` binary at the front of PATH, so the
// slack-cli token source reports itself available here and runs the stub
// rather than a real CLI — which would talk to Slack and wait for a person.
//
// appprovision's own tests point its unexported slackBin var at a stand-in;
// from this package the same seam is reached through PATH, since Available()
// and Token() both resolve the bare name "slack" with exec.LookPath. t.Setenv
// makes any test using this non-parallel, which is what we want anyway.
func stubSlackCLIOnPATH(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "slack"), []byte(body), 0o755),
		"write the stub slack binary")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// slackCLIStubPrinting is a stub CLI that succeeds, printing tok the way
// `slack auth token` prints a service token.
func slackCLIStubPrinting(tok string) string { return "#!/bin/sh\necho " + tok + "\n" }

// newWizardWithProvisioner builds a wizard whose auth.test always succeeds and
// whose provisioning is the supplied fake.
func newWizardWithProvisioner(c appprovision.Client) *slackWizard {
	w := newWizardWithStub(okAuth(), nil)
	w.provisionClient = c
	return w
}

// provisioningAnswers is what a run that asked oap to create the app has
// answered by the time Resolve runs: no tokens, because minting them is what
// Resolve is about to do.
func provisioningAnswers(t *testing.T, source, configToken, channelName string) map[string]string {
	t.Helper()
	preChecked, err := activeCapabilities(slackCapabilityOptions(), nil)
	require.NoError(t, err)
	return map[string]string{
		keyAgentClass:             "demo-agent",
		keyHasSlackApp:            string(routeProvision),
		keyCapabilities:           strings.Join(preChecked, ","),
		keyTokenSource:            source,
		keyConfigToken:            configToken,
		wizardkeys.KeyChannelName: channelName,
	}
}

// TestWizard_Provision_FillsBothTokensAndNeverAsks is the whole point: a run
// that provisions ends with the same Secret a pasted run produces, and was
// never asked for either token.
//
// Both halves are checked, because either alone is satisfiable by a bug: the
// question set proves nothing was ASKED, and the Secret proves the tokens
// nonetheless arrived.
func TestWizard_Provision_FillsBothTokensAndNeverAsks(t *testing.T) {
	fake := &fakeProvisionClient{}
	w := newWizardWithProvisioner(fake)

	in := defaultInput(newAgentClass("demo-agent", "default"))
	in.Seeded = seededAnswers(map[string]string{keyHasSlackApp: string(routeProvision)})

	qs, err := w.Inputs(context.Background(), in)
	require.NoError(t, err, "Inputs")
	for _, q := range qs {
		assert.NotEqual(t, keyBotToken, q.Name, "a provisioned run must not ask for a token it is about to mint")
		assert.NotEqual(t, keyAppToken, q.Name, "nor for the app-level one")
	}

	out, err := resolvedAgentRun(t, w, in, provisioningAnswers(t, appprovision.KeyPaste, "xoxe.xoxp-demo", "my-channel"))
	require.NoError(t, err, "wizard run")

	require.NotNil(t, out.SecretManifest, "SecretManifest")
	assert.Equal(t, validBotToken, string(out.SecretManifest.Data[SecretKeyBotToken]))
	assert.Equal(t, validAppToken, string(out.SecretManifest.Data[SecretKeyAppToken]))
}

// TestWizard_Provision_SendsTheSameScopesItAsksFor: the manifest requests a set
// of scopes and the install grants a set of scopes. They are generated from one
// derivation, and this is what proves the two calls cannot drift apart.
//
// ElementsMatch, not a one-directional Contains loop: Contains only proves
// gotBotScopes is a SUBSET of the manifest's scopes, so a regression that sent
// Install a strict subset of what the manifest requested — an accidental
// slice or filter — would pass trivially, since every element of a subset is
// contained in the superset it came from. ElementsMatch requires the same set
// in both directions.
func TestWizard_Provision_SendsTheSameScopesItAsksFor(t *testing.T) {
	fake := &fakeProvisionClient{}
	in := defaultInput(newAgentClass("demo-agent", "default"))

	_, err := resolvedAgentRun(t, newWizardWithProvisioner(fake), in,
		provisioningAnswers(t, appprovision.KeyPaste, "xoxe.xoxp-demo", "my-channel"))
	require.NoError(t, err, "wizard run")

	require.NotEmpty(t, fake.gotBotScopes, "install must be granted scopes")
	// manifestBotScopes (manifest_test.go) parses via sigs.k8s.io/yaml, which
	// accepts JSON as a syntactic subset — the same helper other manifest tests
	// use, reused here rather than a second parser for the same document.
	assert.ElementsMatch(t, manifestBotScopes(t, fake.gotManifestJSON), fake.gotBotScopes,
		"the scopes Install was granted must be EXACTLY the scopes the manifest requested — neither a subset nor a superset")
	assert.Equal(t, "A0DEMO", fake.gotAppID, "install targets the app create returned")
}

// TestWizard_Provision_ConfigTokenNeverLeavesTheRun: the token can create and
// delete any of the user's Slack apps, so it must reach nothing that outlives
// the run and nothing the user's scrollback keeps.
//
// Every surface a value can escape through is checked, not just the Secret:
// the summary is left behind in plain scrollback after the alt-screen is
// released, the next-steps notes are printed after it, and the manifests are
// what is written to the cluster. A leak into any one of them is the same
// disclosure.
func TestWizard_Provision_ConfigTokenNeverLeavesTheRun(t *testing.T) {
	const configToken = "xoxe.xoxp-demo"
	fake := &fakeProvisionClient{}
	in := defaultInput(newAgentClass("demo-agent", "default"))

	out, err := resolvedAgentRun(t, newWizardWithProvisioner(fake), in,
		provisioningAnswers(t, appprovision.KeyPaste, configToken, "my-channel"))
	require.NoError(t, err, "wizard run")
	// Without this the assertions below could all hold for a run that never
	// authenticated with the token at all.
	require.Equal(t, configToken, fake.gotToken, "the run must have authenticated with the pasted token")

	assertNoConfigToken(t, out, configToken)
}

// assertNoConfigToken checks every surface one run's output could carry the
// app-configuration token out on.
func assertNoConfigToken(t *testing.T, out channelkinds.WizardOutput, configToken string) {
	t.Helper()
	require.NotEmpty(t, configToken, "a blank needle would make every assertion below vacuous")

	if out.SecretManifest != nil {
		for k, v := range out.SecretManifest.Data {
			assert.NotContains(t, string(v), configToken, "Secret key %q must not carry the config token", k)
		}
	}
	assert.NotContains(t, renderedSummary(t, out.Summary), configToken,
		"the summary is left in plain scrollback, so it must not carry the config token")
	for _, n := range out.Notes {
		assert.NotContains(t, n, configToken, "next-steps note must not carry the config token")
	}
	for label, obj := range map[string]any{
		"Channel":          out.ChannelManifest,
		"Secret":           out.SecretManifest,
		"capability patch": out.CapabilityPatch,
	} {
		if obj == nil {
			continue
		}
		raw, err := yaml.Marshal(obj)
		require.NoError(t, err, "marshal the %s manifest", label)
		assert.NotContains(t, string(raw), configToken, "the %s manifest must not carry the config token", label)
	}
}

// TestProvision_ConfigTokenNeverLeavesAFailedRun: a failure is where a secret
// usually escapes — into an error string that is then printed, logged and
// often pasted into an issue. All three provisioning failure modes are
// covered.
//
// The token-source failure uses a stub CLI that exits non-zero, which is the
// case where a token was pasted AND discarded: the source mints its own, so a
// leak here would disclose a value the run had no use for.
func TestProvision_ConfigTokenNeverLeavesAFailedRun(t *testing.T) {
	const configToken = "xoxe.xoxp-demo-secret"

	cases := []struct {
		name    string
		fake    *fakeProvisionClient
		source  string
		wantErr string
	}{
		{
			name:    "create fails",
			fake:    &fakeProvisionClient{createErr: errors.New("invalid_manifest")},
			source:  appprovision.KeyPaste,
			wantErr: "invalid_manifest",
		},
		{
			name:    "install fails, leaving an app behind",
			fake:    &fakeProvisionClient{installErr: errors.New("internal_error")},
			source:  appprovision.KeyPaste,
			wantErr: "internal_error",
		},
		{
			name:    "the token source itself fails",
			fake:    &fakeProvisionClient{},
			source:  appprovision.KeySlackCLI,
			wantErr: "not logged in",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubSlackCLIOnPATH(t, "#!/bin/sh\necho 'not logged in' >&2\nexit 1\n")

			in := defaultInput(newAgentClass("demo-agent", "default"))
			// The stub CLI on PATH IS this run's operator shell: the source
			// that shells out is only reachable from one, so a run driving it
			// has to say so the way `oap channel create` does.
			in.OperatorShell = true
			_, err := newWizardWithProvisioner(tc.fake).Resolve(context.Background(), in,
				provisioningAnswers(t, tc.source, configToken, "my-channel"))

			require.Error(t, err, "a provisioning failure ends the run")
			assert.Contains(t, err.Error(), tc.wantErr, "the reason must survive")
			assert.NotContains(t, err.Error(), configToken,
				"the returned error must not carry the config token — it is printed, logged and often pasted into an issue")
		})
	}
}

// TestWizard_Provision_SlackCLISourceMintsItsOwnToken: choosing a source that
// mints its own token must not then demand one. An empty app-config-token
// answer is legitimate on that source, and the token the run authenticates
// with must be the one the CLI printed.
//
// fake.gotToken is what proves the CLI produced it: it is the only trace a
// source that collects nothing leaves.
func TestWizard_Provision_SlackCLISourceMintsItsOwnToken(t *testing.T) {
	stubSlackCLIOnPATH(t, slackCLIStubPrinting("xoxe.xoxp-from-cli"))

	fake := &fakeProvisionClient{}
	in := defaultInput(newAgentClass("demo-agent", "default"))
	in.OperatorShell = true

	out, err := resolvedAgentRun(t, newWizardWithProvisioner(fake), in,
		provisioningAnswers(t, appprovision.KeySlackCLI, "", "my-channel"))
	require.NoError(t, err, "a run that chose the CLI source must complete without pasting anything")

	assert.Equal(t, "xoxe.xoxp-from-cli", fake.gotToken,
		"the token must be the one the CLI printed, not something the user typed")
	require.NotNil(t, out.SecretManifest, "SecretManifest")
	assert.Equal(t, validBotToken, string(out.SecretManifest.Data[SecretKeyBotToken]),
		"provisioning still mints both tokens")
}

// TestProvisionInputs_TheSlackCLISourceIsOfferedOnlyToTheOperatorsOwnShell is
// the terminal-host assumption that survived the migration under another name.
//
// The contract stopped naming a tui type, but the enum an operator READS for
// "How should oap authenticate with Slack?" was still computed by looking for
// a `slack` binary on the PATH of whatever process called Inputs. Under admind
// that is the webd container: an image that happens to ship the CLI would
// offer a route that then execs a binary in a pod and blocks on Slack's ticket
// flow, waiting for a person to type a challenge code at a terminal that is
// not there — and WizardInput.NonInteractive does not catch it, because a web
// form correctly sets it false.
//
// The stub is on the PATH for BOTH halves, which is what makes this a test of
// the client capability rather than of the probe: the binary is present either
// way, and only OperatorShell differs.
func TestProvisionInputs_TheSlackCLISourceIsOfferedOnlyToTheOperatorsOwnShell(t *testing.T) {
	sourcesOffered := func(t *testing.T, operatorShell bool) []string {
		t.Helper()
		in := defaultInput(newAgentClass("demo-agent", "default"))
		in.OperatorShell = operatorShell
		in.Seeded = map[string]string{keyHasSlackApp: string(routeProvision)}

		qs, err := kindWizard(t).Inputs(context.Background(), in)
		require.NoError(t, err)
		for _, q := range qs {
			if q.Name == keyTokenSource {
				return q.Enum
			}
		}
		t.Fatalf("the provisioning route must declare %q", keyTokenSource)
		return nil
	}

	stubSlackCLIOnPATH(t, slackCLIStubPrinting("xoxe.xoxp-from-cli"))

	assert.Equal(t, []string{appprovision.KeyPaste, appprovision.KeySlackCLI},
		sourcesOffered(t, true),
		"the operator's own shell can run the CLI, and a binary on ITS path is a fact about their machine")
	assert.Equal(t, []string{appprovision.KeyPaste},
		sourcesOffered(t, false),
		"anything else must be offered only the source that needs no terminal, whatever is installed on the serving host")
}

// TestWizardResolve_TheSlackCLISourceIsRefusedOffTheOperatorsShell is the
// fail-closed half: the answer can also arrive as a flag, or from a client
// that never called Inputs, so the gate cannot live only in the question set.
//
// The stub CLI SUCCEEDS here, which is what makes the test a real negative:
// without the guard the source mints a token, provisioning proceeds, and a
// Slack app gets created from a subprocess run inside a serving container.
func TestWizardResolve_TheSlackCLISourceIsRefusedOffTheOperatorsShell(t *testing.T) {
	stubSlackCLIOnPATH(t, slackCLIStubPrinting("xoxe.xoxp-from-cli"))

	fake := &fakeProvisionClient{}
	in := defaultInput(newAgentClass("demo-agent", "default"))
	// Left false: this is a client that is NOT the operator's shell, and
	// nobody claimed otherwise.
	_, err := newWizardWithProvisioner(fake).Resolve(context.Background(), in,
		provisioningAnswers(t, appprovision.KeySlackCLI, "", "my-channel"))

	require.Error(t, err, "a source that runs on the operator's machine must be refused off it")
	assert.Contains(t, err.Error(), appprovision.KeySlackCLI, "the refusal must name the source")
	assert.Contains(t, err.Error(), appprovision.KeyPaste, "and the one that would work instead")
	assert.False(t, fake.createCalled, "nothing may be created from a token minted on the wrong machine")
}

// TestWizard_Provision_PasteSourceStillRequiresAToken is the other direction of
// the same rule: making the token question optional (a source that mints its
// own ignores it, and which source that will be is not known when the question
// set is declared — see agentCredentialQuestions) must not have made the paste
// source toothless. A blank paste has to be refused, and by name.
func TestWizard_Provision_PasteSourceStillRequiresAToken(t *testing.T) {
	fake := &fakeProvisionClient{}
	in := defaultInput(newAgentClass("demo-agent", "default"))

	_, err := newWizardWithProvisioner(fake).Resolve(context.Background(), in,
		provisioningAnswers(t, appprovision.KeyPaste, "", "my-channel"))

	require.Error(t, err, "an empty paste must be refused rather than sent to Slack")
	assert.Contains(t, err.Error(), "configuration token",
		"the refusal must say what is missing")
	assert.False(t, fake.createCalled, "and nothing may be created on an unauthenticated run")
}

// TestWizard_Provision_TokenQuestionIsStillAskedWhenOnlyTheSourceIsSeeded pins
// the boundary of "this run has nothing left to ask": a run missing exactly one
// piece must still be asked for it.
func TestWizard_Provision_TokenQuestionIsStillAskedWhenOnlyTheSourceIsSeeded(t *testing.T) {
	cases := []struct {
		name   string
		seeded map[string]string
	}{
		{
			name: "token source seeded, config token is not: still asks for the token",
			seeded: map[string]string{
				keyHasSlackApp: string(routeProvision),
				keyTokenSource: appprovision.KeyPaste,
			},
		},
		{
			name:   "neither seeded: still asks for both",
			seeded: map[string]string{keyHasSlackApp: string(routeProvision)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := defaultInput(newAgentClass("demo-agent", "default"))
			in.Seeded = seededAnswers(tc.seeded)

			qs, err := newWizardWithProvisioner(&fakeProvisionClient{}).Inputs(context.Background(), in)
			require.NoError(t, err, "Inputs")

			names := make([]string, 0, len(qs))
			for _, q := range qs {
				names = append(names, q.Name)
			}
			assert.Contains(t, names, keyConfigToken,
				"a partially-seeded run must still be asked for the missing piece")
		})
	}
}

// TestWizard_Provision_InstallWithoutAnAppIDFallsBackToTheOneAskedFor:
// apps.developerInstall is undocumented, so a response missing app_id is a
// shape it could take without notice. An empty app ID is not a harmless gap —
// it is the same value a pasted-token run leaves, so it would silently
// reclassify a provisioned run as a pasted one: no ID on the Channel, and the
// "save your tokens, they cannot be retrieved later" warning shown to a user
// who was never given a token to save.
func TestWizard_Provision_InstallWithoutAnAppIDFallsBackToTheOneAskedFor(t *testing.T) {
	fake := &fakeProvisionClient{omitInstallAppID: true}
	in := defaultInput(newAgentClass("demo-agent", "default"))

	out, err := resolvedAgentRun(t, newWizardWithProvisioner(fake), in,
		provisioningAnswers(t, appprovision.KeyPaste, "xoxe.xoxp-demo", "my-channel"))
	require.NoError(t, err, "wizard run")

	require.NotNil(t, out.ChannelManifest.Spec.Slack, "slack config")
	assert.Equal(t, "A0DEMO", out.ChannelManifest.Spec.Slack.AppID,
		"the ID Install was called with is authoritative for which app these tokens belong to, and nothing in Slack's API could recover it later")

	joined := strings.Join(out.Notes, "\n")
	assert.Contains(t, joined, "A0DEMO", "and the next-steps note names the app that was created")
	assert.NotContains(t, joined, "Save the bot-token and app-token in your password manager",
		"that warning is for a run whose tokens the user pasted and holds the only copy of")
}

// TestWizard_Provision_InstallFailureNamesTheOrphanedApp: Create can succeed
// while Install fails — the app exists in the workspace's app list with no
// tokens — and the error has to name enough for a user to find it rather than
// create a second app for the same agent. No Slack API lists a user's apps, so
// this message is the only record of it there will ever be.
func TestWizard_Provision_InstallFailureNamesTheOrphanedApp(t *testing.T) {
	fake := &fakeProvisionClient{installErr: &appprovision.APIError{
		Method: "apps.developerInstall", Code: "app_approval_request_eligible",
	}}
	in := defaultInput(newAgentClass("demo-agent", "default"))

	_, err := newWizardWithProvisioner(fake).Resolve(context.Background(), in,
		provisioningAnswers(t, appprovision.KeyPaste, "xoxe.xoxp-demo", "my-channel"))

	require.Error(t, err, "an install that failed must not be reported as a success")
	assert.Contains(t, err.Error(), "A0DEMO",
		"the app ID is the only handle the operator has on an app nothing can list")
	assert.Contains(t, err.Error(), "app_approval_request_eligible",
		"and Slack's own reason must survive, or the operator cannot tell what to fix")
}

// TestWizard_Provision_TokensAlreadyPresent_SkipsProvisioning: a caller who
// provisioned once, saved both tokens, and re-runs with slackapp=provision
// still set must not mint a SECOND Slack app for the same agent.
//
// createCalled/installCalled staying false is the actual proof of the skip; a
// passing run with no such assertion would not distinguish "skipped
// provisioning" from "provisioned again and happened to return the same
// tokens".
func TestWizard_Provision_TokensAlreadyPresent_SkipsProvisioning(t *testing.T) {
	fake := &fakeProvisionClient{}
	w := newWizardWithProvisioner(fake)

	in := defaultInput(newAgentClass("demo-agent", "default"))
	in.Seeded = seededAnswers(map[string]string{
		keyHasSlackApp: string(routeProvision),
		keyBotToken:    validBotToken,
		keyAppToken:    validAppToken,
	})

	answers := provisioningAnswers(t, appprovision.KeyPaste, "", "my-channel")
	answers[keyBotToken] = validBotToken
	answers[keyAppToken] = validAppToken

	out, err := resolvedAgentRun(t, w, in, answers)
	require.NoError(t, err, "a run with both tokens already in hand must complete even with slackapp=provision still set")

	assert.False(t, fake.createCalled, "Create must not be called — both tokens were already present")
	assert.False(t, fake.installCalled, "nor Install — calling it would mint a second app for the same agent")

	require.NotNil(t, out.SecretManifest, "SecretManifest")
	assert.Equal(t, validBotToken, string(out.SecretManifest.Data[SecretKeyBotToken]),
		"the supplied bot token must survive unchanged")
	assert.Equal(t, validAppToken, string(out.SecretManifest.Data[SecretKeyAppToken]),
		"so must the supplied app token")
}
