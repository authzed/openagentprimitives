package slack

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	slackapi "github.com/slack-go/slack"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/appprovision"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
)

// stubAuthTester returns the configured response or error.
type stubAuthTester struct {
	resp *slackapi.AuthTestResponse
	err  error
}

func (s *stubAuthTester) AuthTestContext(_ context.Context) (*slackapi.AuthTestResponse, error) {
	return s.resp, s.err
}

// newWizardWithStub builds a wizard whose auth.test always answers the same
// way, whatever token it is handed.
func newWizardWithStub(resp *slackapi.AuthTestResponse, authErr error) *slackWizard {
	return &slackWizard{authFactory: func(string) authTester {
		return &stubAuthTester{resp: resp, err: authErr}
	}}
}

func newFakeK8s(objs ...runtime.Object) *fake.ClientBuilder {
	scheme := runtime.NewScheme()
	_ = spiceboxv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...)
}

const validBotToken = "xoxb-1234567890123-1234567890123-abcdefghijklmnopqrstuvwx"
const validAppToken = "xapp-1-AAAAAAAAA-1234567890123-abcdefghijklmnopqrstuvwxyz"

func newAgentClass(name, namespace string) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
}

func okAuth() *slackapi.AuthTestResponse {
	return &slackapi.AuthTestResponse{Team: "Demo Workspace", User: "demo-bot", UserID: "U0DEMO123"}
}

func defaultInput(objs ...runtime.Object) channelkinds.WizardInput {
	return channelkinds.WizardInput{K8s: newFakeK8s(objs...).Build(), Namespace: "default"}
}

// resolvedAgentRun drives the two steps a completed agent-flow run takes after
// its questions are answered — Resolve, then Result — merging what Resolve
// derived into the answer map exactly as the CLI dispatcher does.
//
// It exists so a test of the OUTPUT exercises the same sequence a real run
// does, rather than hand-writing the derived answers auth.test produces and
// then asserting they came back.
func resolvedAgentRun(
	t *testing.T,
	w *slackWizard,
	in channelkinds.WizardInput,
	answers map[string]string,
) (channelkinds.WizardOutput, error) {
	t.Helper()
	derived, err := w.Resolve(context.Background(), in, answers)
	if err != nil {
		return channelkinds.WizardOutput{}, err
	}
	merged := make(map[string]string, len(answers)+len(derived))
	for k, v := range answers {
		merged[k] = v
	}
	for k, v := range derived {
		merged[k] = v
	}
	return w.Result(in, merged)
}

// typedAgentAnswers is what an operator who already has a Slack app types: the
// four answers the agent flow asks for on that route, with the identity left
// to Resolve.
func typedAgentAnswers(t *testing.T, channelName string) map[string]string {
	t.Helper()
	preChecked, err := activeCapabilities(slackCapabilityOptions(), nil)
	require.NoError(t, err)
	return map[string]string{
		keyAgentClass:             "demo-agent",
		keyHasSlackApp:            string(routeHave),
		keyCapabilities:           strings.Join(preChecked, ","),
		keyBotToken:               validBotToken,
		keyAppToken:               validAppToken,
		wizardkeys.KeyChannelName: channelName,
	}
}

// TestWizard_HappyPath_BuildsSecretAndChannel is the whole point of the flow:
// the answers become a Secret holding both tokens and a Channel bound to the
// chosen AgentClass, carrying the bot user ID auth.test reported.
func TestWizard_HappyPath_BuildsSecretAndChannel(t *testing.T) {
	in := defaultInput(newAgentClass("demo-agent", "default"))
	out, err := resolvedAgentRun(t, newWizardWithStub(okAuth(), nil), in,
		typedAgentAnswers(t, "my-channel"))
	require.NoError(t, err, "wizard run")

	require.NotNil(t, out.SecretManifest, "SecretManifest")
	sec := out.SecretManifest
	assert.Equal(t, "my-channel-creds", sec.Name, "secret name follows the Channel name")
	assert.Equal(t, "default", sec.Namespace, "secret namespace")
	assert.Equal(t, validBotToken, string(sec.Data[SecretKeyBotToken]), "secret bot-token")
	assert.Equal(t, validAppToken, string(sec.Data[SecretKeyAppToken]), "secret app-token")

	require.NotNil(t, out.ChannelManifest, "ChannelManifest")
	ch := out.ChannelManifest
	assert.Equal(t, "my-channel", ch.Name, "channel name")
	assert.Equal(t, "default", ch.Namespace, "channel namespace")
	assert.Equal(t, "slack", ch.Spec.Kind, "channel kind")
	assert.Equal(t, "demo-agent", ch.Spec.AgentClass, "agentClass binding")
	assert.Equal(t, "my-channel-creds", ch.Spec.CredentialsRef.SecretName, "credentialsRef")
	require.NotNil(t, ch.Spec.Slack, "slack config")
	assert.Equal(t, "socket", ch.Spec.Slack.Mode, "socket mode")
	assert.Equal(t, "U0DEMO123", ch.Spec.Slack.BotUserID, "BotUserID from auth.test")

	assert.NotEmpty(t, out.Notes, "post-apply guidance")
}

// TestWizard_UsesTheAgentClassActuallyChosen pins that the ANSWER decides the
// binding, not the enum's own pre-selection.
//
// The namespace holds two classes and the answer names the second, so a
// Result that fell back to the listing's first entry — which is what an enum
// pre-selects, and what every other test in this file happens to answer with
// — would bind the wrong agent. The capability patch is asserted too: it
// targets an AgentClass by name and writes an explicit enabled:false for every
// capability, so aiming it at the wrong one revokes grants on a class the
// operator never touched.
func TestWizard_UsesTheAgentClassActuallyChosen(t *testing.T) {
	in := defaultInput(
		newAgentClass("alpha-agent", "default"),
		newAgentClass("demo-agent", "default"),
	)
	answers := typedAgentAnswers(t, "slack-demo-agent")
	answers[keyAgentClass] = "demo-agent"

	out, err := resolvedAgentRun(t, newWizardWithStub(okAuth(), nil), in, answers)
	require.NoError(t, err, "wizard run")

	require.NotNil(t, out.ChannelManifest)
	assert.Equal(t, "demo-agent", out.ChannelManifest.Spec.AgentClass,
		"the second option must be bindable, not just the first")
	require.NotNil(t, out.CapabilityPatch)
	assert.Equal(t, "demo-agent", out.CapabilityPatch.GetName(),
		"the capability patch must target the class actually chosen")
}

// TestWizard_DerivesDefaultChannelNameFromAgent: accepting the offered name
// yields "slack-<agentclass>", and the Secret follows it.
//
// The name is read out of the question's own Default rather than transcribed,
// so a default that silently changed fails here instead of leaving both halves
// agreeing on a stale value.
func TestWizard_DerivesDefaultChannelNameFromAgent(t *testing.T) {
	in := defaultInput(newAgentClass("demo-agent", "default"))
	in.Seeded = seededAnswers(map[string]string{keyHasSlackApp: string(routeHave)})

	qs, err := kindWizard(t).Inputs(context.Background(), in)
	require.NoError(t, err)

	var offered string
	for _, q := range qs {
		if q.Name == wizardkeys.KeyChannelName {
			s, ok := q.Default.(string)
			require.True(t, ok, "the name question must offer a string default to accept")
			offered = s
		}
	}
	require.Equal(t, "slack-demo-agent", offered, "the offered name derives from the bound AgentClass")

	answers := typedAgentAnswers(t, offered)
	out, err := resolvedAgentRun(t, newWizardWithStub(okAuth(), nil), in, answers)
	require.NoError(t, err, "wizard run")

	require.NotNil(t, out.ChannelManifest)
	assert.Equal(t, "slack-demo-agent", out.ChannelManifest.Name, "derived Channel name")
	require.NotNil(t, out.SecretManifest)
	assert.Equal(t, "slack-demo-agent-creds", out.SecretManifest.Name, "derived Secret name")
}

// TestWizard_SummaryRecordsWhatWasDecided: the summary is the record that
// survives in scrollback after the alt-screen is released, so every decision
// has to be in it — including the ones nobody was asked about.
//
// The LABELS are asserted as an ordered list, not just looked up: the summary
// is read top to bottom, and a line silently dropped or reordered changes what
// the operator reads. What each credential line CARRIES is
// TestWizardSummary_AgentFlowNeverPutsARawTokenInScrollback's claim.
func TestWizard_SummaryRecordsWhatWasDecided(t *testing.T) {
	in := defaultInput(newAgentClass("demo-agent", "default"))
	out, err := resolvedAgentRun(t, newWizardWithStub(okAuth(), nil), in,
		typedAgentAnswers(t, "my-channel"))
	require.NoError(t, err, "wizard run")

	labels := make([]string, 0, len(out.Summary))
	values := map[string]string{}
	for _, n := range out.Summary {
		labels = append(labels, n.Label)
		values[n.Label] = n.Value
	}
	assert.Equal(t,
		[]string{"AgentClass", "Capabilities", "Bot token", "App token", "Slack team", "Bot", "Channel"},
		labels, "every decision, in the order the operator made it")

	assert.Equal(t, "demo-agent", values["AgentClass"], "which agent was bound")
	assert.Equal(t, "my-channel", values["Channel"], "what the Channel is called")
	assert.Equal(t, "Demo Workspace", values["Slack team"], "which workspace answered auth.test")
	assert.Equal(t, "@demo-bot (U0DEMO123)", values["Bot"], "which bot user the tokens belong to")
	assert.NotContains(t, values["Bot"], "@ (", "no half-filled identity may be rendered")
}

// pastedRunNotes is the next-steps block a run whose operator pasted their own
// tokens produces, driven all the way through Resolve → Result → agentOutput
// rather than by calling postSetupNotes directly: which notes a run gets is
// decided by what agentOutput passes it (an empty appID, here), so a direct
// call would assert the note text while proving nothing about who receives it.
func pastedRunNotes(t *testing.T) string {
	t.Helper()
	in := defaultInput(newAgentClass("demo-agent", "default"))
	out, err := resolvedAgentRun(t, newWizardWithStub(okAuth(), nil), in,
		typedAgentAnswers(t, "my-channel"))
	require.NoError(t, err, "wizard run")
	return strings.Join(out.Notes, "\n")
}

// provisionedRunNotes is the same block for a run that asked oap to create and
// install the app, so the two routes are compared on identical footing.
func provisionedRunNotes(t *testing.T) string {
	t.Helper()
	in := defaultInput(newAgentClass("demo-agent", "default"))
	out, err := resolvedAgentRun(t, newWizardWithProvisioner(&fakeProvisionClient{}), in,
		provisioningAnswers(t, appprovision.KeyPaste, "xoxe.xoxp-demo", "my-channel"))
	require.NoError(t, err, "wizard run")
	return strings.Join(out.Notes, "\n")
}

// TestPostSetupNotes_PastedRunKeepsTheTokenWarning is the positive half of
// TestWizard_Provision_InstallWithoutAnAppIDFallsBackToTheOneAskedFor's
// NotContains: a user who pasted their own tokens holds the only copy, and this
// note is the sole notice they will get that the Secret cannot give them back.
//
// Without it that NotContains passes vacuously the moment the note is dropped
// from every route.
func TestPostSetupNotes_PastedRunKeepsTheTokenWarning(t *testing.T) {
	assert.Contains(t, pastedRunNotes(t),
		"Save the bot-token and app-token in your password manager — they cannot be retrieved later from the Secret.",
		"the pasted-token route is the one route where the operator holds the only copy")
}

// TestPostSetupNotes_PastedRunKeepsTheHandMigrationAdvice: a pre-existing app
// really might declare assistant_view, and really might have none of the three
// 'is thinking…' prerequisites — oap built no manifest on this route, so it
// touched none of them.
func TestPostSetupNotes_PastedRunKeepsTheHandMigrationAdvice(t *testing.T) {
	joined := pastedRunNotes(t)

	assert.Contains(t, joined, "'Agents & AI Apps' enabled",
		"the workspace admin toggle is the user's, on every route")
	assert.Contains(t, joined, "the app needs its 'Agent or Assistant' feature ON, and the bot needs the assistant:write scope",
		"unlike a provisioned run, oap requested neither for a hand-built app")
	assert.Contains(t, joined, "still declares assistant_view: change it to agent_view",
		"an app that predates the inline Messages-tab surface has to be migrated by hand")
}

// TestPostSetupNotes_NoMembershipEventsReinstallNotice is the revert half of
// the whole-branch review's Major 4: the note this replaces advertised a
// reinstall for "real-time membership sync" that no code path consumes —
// member_joined_channel/member_left_channel are not even in the manifest any
// more (see TestAppManifestOmitsUnwiredMembershipEvents), so a note telling
// an operator to reinstall for them would send them chasing a benefit that
// does not exist. Asserted on both routes: the note must be gone
// everywhere, not merely un-added to the one route it used to appear on.
func TestPostSetupNotes_NoMembershipEventsReinstallNotice(t *testing.T) {
	for _, notes := range []string{pastedRunNotes(t), provisionedRunNotes(t)} {
		assert.NotContains(t, notes, "member_joined_channel",
			"the Invalidator this note advertised is unwired; the note must not promise a benefit nothing delivers")
		assert.NotContains(t, notes, "real-time membership sync",
			"same note, same reason")
	}
}

// TestPostSetupNotes_DoNotAskForWorkTheCLIDoes: `oap channel create` watches
// the Channel it applied and reports Connected/ScopesValid itself, so a note
// telling the user to run the command the CLI just ran is work handed back —
// and a wall of them is a block nobody reads. Asserted on both routes.
func TestPostSetupNotes_DoNotAskForWorkTheCLIDoes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		notes func(t *testing.T) string
	}{
		{name: "pasted-token run: no note sends the user to `oap channel show`", notes: pastedRunNotes},
		{name: "provisioned run: no note sends the user to `oap channel show`", notes: provisionedRunNotes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joined := tc.notes(t)
			assert.NotContains(t, joined, "oap channel show",
				"the CLI watches the Channel and names the follow-up command on any non-green outcome; the note is the work it now does")
			assert.NotContains(t, joined, "Connected=True",
				"what the conditions say is the watch's report, not a thing to go read")
		})
	}
}

// TestRouteOfAnswer_ReadsTheWholeVocabulary pins the compatibility contract:
// the two values --answer slackapp=… already accepted keep meaning what they
// meant, the new one is additive, and anything else lands on the safe floor.
func TestRouteOfAnswer_ReadsTheWholeVocabulary(t *testing.T) {
	cases := []struct {
		name   string
		answer string
		want   appRoute
	}{
		{name: `"true": the operator already has an app`, answer: "true", want: routeHave},
		{name: `"false": show the manifest`, answer: "false", want: routeManual},
		{name: `"provision": create the app for me`, answer: "provision", want: routeProvision},
		{name: "surrounding whitespace is trimmed, not read as a fourth value", answer: "  provision  ", want: routeProvision},
		{name: "unanswered: show the manifest, the safe floor", answer: "", want: routeManual},
		{name: "a value nothing offers: the safe floor, never an app that does not exist", answer: "yes", want: routeManual},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, routeOfAnswer(tc.answer))
		})
	}
}

// TestManualRoute_SaveOutcomesAreAllReported: the manifest file is a
// convenience, so a failed write must not end the run — but it must never be
// silent either, and a file that was already there must not be replaced
// without saying so.
//
// Driven through manualRouteRefusal, which is where this text is produced and
// the file is written: the message that ends the run IS the deliverable on
// that route (see its doc), so what it says about the file is the only report
// the operator gets.
func TestManualRoute_SaveOutcomesAreAllReported(t *testing.T) {
	const fileName = "slack-app-manifest-demo-agent.yaml"

	cases := []struct {
		name string
		// setup returns the directory manualRouteRefusal writes into.
		setup func(t *testing.T) string
		want  []string
		deny  []string
	}{
		{
			name:  "nothing there yet: the path is named and nothing is said about replacing",
			setup: func(t *testing.T) string { return t.TempDir() },
			want:  []string{fileName},
			deny:  []string{"replac"},
		},
		{
			name: "a file of that name already there: the message says it was replaced",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(dir, fileName), []byte("older\n"), 0o644))
				return dir
			},
			want: []string{fileName, "replac"},
		},
		{
			name: "the file cannot be written: the reason is in the message and the manifest still is too",
			setup: func(t *testing.T) string {
				// A regular file standing where the directory should be: every
				// write into it fails, with no permission juggling to undo.
				blocked := filepath.Join(t.TempDir(), "not-a-directory")
				require.NoError(t, os.WriteFile(blocked, []byte("x"), 0o644))
				return blocked
			},
			want: []string{"not saved", fileName},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := defaultInput(newAgentClass("demo-agent", "default"))
			in.WorkingDir = tc.setup(t)

			_, err := newWizardWithStub(okAuth(), nil).Resolve(context.Background(), in, map[string]string{
				keyAgentClass:   "demo-agent",
				keyHasSlackApp:  string(routeManual),
				keyCapabilities: "",
			})
			require.Error(t, err, "the manual route always ends the run by handing over the manifest")

			for _, want := range tc.want {
				assert.Contains(t, err.Error(), want, "the message must say what happened to the file")
			}
			for _, deny := range tc.deny {
				assert.NotContains(t, err.Error(), deny, "the message must not claim something that did not happen")
			}
			assert.Contains(t, err.Error(), "display_information",
				"the manifest itself is in the message either way")
		})
	}
}

// TestKindWizardIsWired: `oap channel create --kind slack` resolves the kind's
// Wizard, so it must be the real flow rather than a stand-in, and it must
// carry the production Slack client wiring.
func TestKindWizardIsWired(t *testing.T) {
	w, ok := (&Kind{}).Wizard().(*slackWizard)
	require.True(t, ok, "Kind.Wizard must return the Slack wizard")
	require.NotNil(t, w.authFactory, "the wizard must be wired to a real Slack client factory")
	assert.NotNil(t, w.authFactory(validBotToken), "the factory must build an auth tester")
	require.NotNil(t, w.provisionClient, "and to a real app-provisioning client")
}

func TestSafePrefix(t *testing.T) {
	assert.Equal(t, "xoxb-1", safePrefix("xoxb-12345"), "safePrefix")
	assert.Equal(t, "abc", safePrefix("abc"), "safePrefix short")
}

// TestAppSetupStepsAreRenderedByteForByte pins the two blocks that are now
// shared with the credential-setup flow
// (pkg/platform/identity/setup/builtins/slack_bot_token), which renders them
// under its own numbering.
//
// Byte-for-byte, and by EQUALITY rather than by containment, because every
// other assertion on this text is a substring: the quoted strings are Slack's
// own UI labels, which an operator matches against the screen in front of
// them, and the indentation is what keeps a manifest line inside the note
// budget huh hard-wraps at. A reflow that a Contains check would sail past is
// exactly the edit that breaks either one, and with two callers there is no
// longer one screen to eyeball.
func TestAppSetupStepsAreRenderedByteForByte(t *testing.T) {
	// The fixture ends with a newline because every real manifest does
	// (botScopesYAML's last line), and that is where the blank line separating
	// this step from the next one comes from: the manifest's own newline plus
	// the one written after it. A fixture without it renders the next step
	// flush against the YAML, which is what this comment exists to stop
	// someone "fixing" in the wrong place.
	assert.Equal(t,
		"  2. Create the app from this manifest:\n"+
			"       https://api.slack.com/apps → 'Create New App'\n"+
			"       → 'From an app manifest'. Pick your workspace,\n"+
			"       then paste:\n"+
			"\n"+
			"<manifest>\n"+
			"\n",
		CreateAppFromManifestStep(2, "<manifest>\n"),
		"the manifest is written verbatim, with no indent of its own, and one blank line either side")

	assert.Equal(t,
		"  5. Install to the workspace:\n"+
			"       'Install App' → 'Install to <Workspace>' →\n"+
			"       Allow, then copy the bot token (xoxb-) shown\n"+
			"       after install.\n",
		InstallAndCopyBotTokenStep(5))

	// The number is the caller's, and it is the only thing that varies: the
	// token-only app has no workspace prerequisite ahead of it, so it creates
	// at step 1 and installs at step 2.
	assert.True(t, strings.HasPrefix(CreateAppFromManifestStep(1, "m\n"), "  1. "))
	assert.True(t, strings.HasPrefix(InstallAndCopyBotTokenStep(2), "  2. "))
}

// TestChannelSetupStepsStillReadInOrder is the other half of the extraction:
// the shared blocks land in the channel wizard's own instructions at the
// numbers they always had, separated the way they always were. The extraction
// was meant to be invisible here.
func TestChannelSetupStepsStillReadInOrder(t *testing.T) {
	steps := appSetupSteps("<manifest>\n", "<come back>", "", "")
	for _, want := range []string{
		"  2. Create the app from this manifest:",
		"<manifest>\n\n  3. Enable the AI Agent feature",
		"       after install.\n\n  6. Generate the app-level token",
	} {
		assert.Contains(t, steps, want)
	}
}
