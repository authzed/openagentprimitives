package slack

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	slackapi "github.com/slack-go/slack"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
)

// monitoringWizardInput is the WizardInput for a first-time monitoring setup.
func monitoringWizardInput(objs ...runtime.Object) channelkinds.WizardInput {
	return channelkinds.WizardInput{
		K8s:        newFakeK8s(objs...).Build(),
		Namespace:  "default",
		Monitoring: true,
	}
}

// reSetupInput is the WizardInput for reconfiguring an existing monitoring
// Channel whose creds Secret is present — the shape that offers keep-on-blank.
func reSetupInput(existing *spiceboxv1alpha1.Channel, credsExist bool) channelkinds.WizardInput {
	in := monitoringWizardInput()
	in.Existing = existing
	in.CredentialsExist = credsExist
	return in
}

// existingMonitoringChannel builds a prior monitoring Channel for the re-setup
// cases, with the given creds Secret name.
func existingMonitoringChannel(secretName string) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "slack-monitoring-demo", Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind: "slack", Role: spiceboxv1alpha1.ChannelRoleMonitoring,
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: secretName},
			Slack: &spiceboxv1alpha1.SlackChannelConfig{
				BotUserID:      "U01OLDBOT",
				OutputDefaults: &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: "C0OLDCHAN"},
			},
		},
	}
}

// assertNotCalledErr is returned by a stub authTester that must never be
// invoked. A wizard that reaches auth.test on the kept-token path fails the
// run loudly instead of silently producing a plausible-looking result.
var assertNotCalledErr = errors.New("auth.test must not be called when keeping the existing token")

// monitoringAnswers is what a first-time monitoring run has answered by the
// time Resolve runs: the app exists, the bot token, the destination, the name.
// The identity is left to Resolve, which is what asks Slack for it.
func monitoringAnswers(channelName string) map[string]string {
	return map[string]string{
		keyHasSlackApp:            string(routeHave),
		keyBotToken:               validBotToken,
		keyDestinationChannelID:   "C0123ABCDEF",
		wizardkeys.KeyChannelName: channelName,
	}
}

// resolvedMonitoringRun drives Resolve then Result for the monitoring flow,
// merging what Resolve derived exactly as the CLI dispatcher does.
func resolvedMonitoringRun(
	t *testing.T,
	w *slackWizard,
	in channelkinds.WizardInput,
	answers map[string]string,
) (channelkinds.WizardOutput, error) {
	t.Helper()
	require.True(t, in.Monitoring, "this helper drives the monitoring flow")
	return resolvedAgentRun(t, w, in, answers)
}

// offeredDefault is the value an operator accepting question name would answer
// with. Reading it back rather than transcribing it is what lets a "keep what
// is stored" test prove the STORED value was offered, instead of asserting a
// literal that happens to match.
func offeredDefault(t *testing.T, in channelkinds.WizardInput, name string) string {
	t.Helper()
	qs, err := kindWizard(t).Inputs(context.Background(), in)
	require.NoError(t, err, "Inputs")
	for _, q := range qs {
		if q.Name != name {
			continue
		}
		s, ok := q.Default.(string)
		require.Truef(t, ok, "question %q offers no string default to accept", name)
		return s
	}
	t.Fatalf("no question named %q was declared", name)
	return ""
}

// TestWizardMonitoring_HappyPath_BuildsMonitoringChannelAndBotOnlySecret is the
// whole point of the flow: a Role=monitoring Channel with no AgentClass, a
// fixed Slack destination, and a Secret holding the bot token alone — no
// app-level token, because nothing on a monitoring channel listens.
func TestWizardMonitoring_HappyPath_BuildsMonitoringChannelAndBotOnlySecret(t *testing.T) {
	out, err := resolvedMonitoringRun(t, newWizardWithStub(okAuth(), nil),
		monitoringWizardInput(), monitoringAnswers("my-mon"))
	require.NoError(t, err, "wizard run")

	require.NotNil(t, out.ChannelManifest, "ChannelManifest")
	ch := out.ChannelManifest
	assert.Equal(t, "my-mon", ch.Name, "channel name")
	assert.Equal(t, "default", ch.Namespace, "channel namespace")
	assert.Equal(t, "slack", ch.Spec.Kind, "channel kind")
	assert.Equal(t, spiceboxv1alpha1.ChannelRoleMonitoring, ch.Spec.Role, "role=monitoring")
	assert.Empty(t, ch.Spec.AgentClass, "a monitoring channel binds to no agent")
	assert.Equal(t, "my-mon-creds", ch.Spec.CredentialsRef.SecretName, "credentialsRef")
	require.NotNil(t, ch.Spec.Slack, "slack config")
	require.NotNil(t, ch.Spec.Slack.OutputDefaults, "outputDefaults")
	assert.Equal(t, "C0123ABCDEF", ch.Spec.Slack.OutputDefaults.ChannelID, "destination channel ID")
	assert.Equal(t, "U0DEMO123", ch.Spec.Slack.BotUserID, "BotUserID from auth.test")

	require.NotNil(t, out.SecretManifest, "SecretManifest")
	sec := out.SecretManifest
	assert.Equal(t, "my-mon-creds", sec.Name, "secret name follows the Channel name")
	assert.Equal(t, "default", sec.Namespace, "secret namespace")
	assert.Equal(t, validBotToken, string(sec.Data[SecretKeyBotToken]), "secret bot-token")
	assert.NotContains(t, sec.Data, SecretKeyAppToken, "monitoring needs no app-level token")

	assert.False(t, out.ReplaceExisting, "a first setup creates rather than replaces")
	assert.NotEmpty(t, out.Notes, "post-apply guidance")
}

// TestWizardMonitoring_NoDestination_ProducesNoManifests: the destination is
// the one thing a monitoring channel cannot be guessed without, so a run that
// never supplies one must fail rather than emit a Channel that posts nowhere.
func TestWizardMonitoring_NoDestination_ProducesNoManifests(t *testing.T) {
	answers := monitoringAnswers("my-mon")
	answers[keyDestinationChannelID] = ""

	out, err := resolvedMonitoringRun(t, newWizardWithStub(okAuth(), nil),
		monitoringWizardInput(), answers)

	require.Error(t, err, "a run with no destination must not succeed")
	assert.Contains(t, err.Error(), keyDestinationChannelID, "the error must name what is missing")
	assert.Contains(t, err.Error(), "C0123ABCDEF",
		"and must say what the answer looks like: monitoringDestinationGuidance reaches an interactive operator as the question's Description, but a non-interactive one renders no question at all")
	assert.Nil(t, out.ChannelManifest, "no Channel may be produced")
	assert.Nil(t, out.SecretManifest, "no Secret may be produced")
}

// TestWizardMonitoring_NoApp_HandsOverSetupStepsAndStops: a user with no Slack
// app has no token to paste, so the run ends with the steps to create one.
//
// The steps are the DELIVERABLE, handed over as the error that ends the run —
// see monitoringSetupSteps, which opens by saying what happened for exactly
// that reason.
func TestWizardMonitoring_NoApp_HandsOverSetupStepsAndStops(t *testing.T) {
	_, err := kindWizard(t).Inputs(context.Background(), func() channelkinds.WizardInput {
		in := monitoringWizardInput()
		in.Seeded = seededAnswers(map[string]string{keyHasSlackApp: string(routeManual)})
		return in
	}())

	require.Error(t, err, "the no-app path stops before asking for anything")

	handedOver := err.Error()
	assert.Contains(t, handedOver, "chat:write", "the one scope a monitoring bot needs")
	assert.Contains(t, handedOver, "api.slack.com/apps", "where to create the app")
	assert.NotContains(t, handedOver, "connections:write",
		"Socket Mode is not involved: a monitoring channel only posts")
}

// TestWizardMonitoring_ReSetup_ReuseNameKeepToken: re-running setup against an
// existing monitoring Channel reuses its name, keeps the stored channel ID and
// token (nil SecretManifest), sets ReplaceExisting, and never calls auth.test —
// there is no plaintext to test.
//
// The two "keeps" are different mechanisms and are exercised as such: a BLANK
// token means keep, because there is nothing to compare a stored secret
// against; the destination is kept by OFFERING the stored value as the
// question's default, which is read back here rather than transcribed.
func TestWizardMonitoring_ReSetup_ReuseNameKeepToken(t *testing.T) {
	// A stub whose auth.test always fails: reaching it fails the run.
	w := newWizardWithStub(nil, assertNotCalledErr)
	in := reSetupInput(existingMonitoringChannel("slack-monitoring-demo-creds"), true)

	assert.Equal(t, "C0OLDCHAN", offeredDefault(t, in, keyDestinationChannelID),
		"the stored destination must be what a bare accept takes")

	out, err := resolvedMonitoringRun(t, w, in,
		map[string]string{
			keyHasSlackApp:          string(routeHave),
			keyBotToken:             "", // blank: keep the stored one
			keyDestinationChannelID: offeredDefault(t, in, keyDestinationChannelID),
		})
	require.NoError(t, err, "wizard run")

	assert.True(t, out.ReplaceExisting, "must update in place")
	assert.Nil(t, out.SecretManifest, "kept token ⇒ nil SecretManifest")
	require.NotNil(t, out.ChannelManifest)
	assert.Equal(t, "slack-monitoring-demo", out.ChannelManifest.Name, "reuse existing name")
	assert.Equal(t, "slack-monitoring-demo-creds", out.ChannelManifest.Spec.CredentialsRef.SecretName,
		"the ref still names the stored Secret")
	require.NotNil(t, out.ChannelManifest.Spec.Slack)
	require.NotNil(t, out.ChannelManifest.Spec.Slack.OutputDefaults)
	assert.Equal(t, "C0OLDCHAN", out.ChannelManifest.Spec.Slack.OutputDefaults.ChannelID, "keep channel ID")
	assert.Equal(t, "U01OLDBOT", out.ChannelManifest.Spec.Slack.BotUserID, "reuse the stored BotUserID")
}

// TestWizardMonitoring_ReSetup_ReplaceToken: a new token re-runs auth.test and
// writes a fresh SecretManifest, still reusing the Channel name.
func TestWizardMonitoring_ReSetup_ReplaceToken(t *testing.T) {
	w := newWizardWithStub(&slackapi.AuthTestResponse{Team: "Demo Workspace", User: "demo-bot", UserID: "U01NEWBOT"}, nil)

	out, err := resolvedMonitoringRun(t, w,
		reSetupInput(existingMonitoringChannel("slack-monitoring-demo-creds"), true),
		map[string]string{
			keyHasSlackApp:          string(routeHave),
			keyBotToken:             validBotToken,
			keyDestinationChannelID: "C0NEWCHAN",
		})
	require.NoError(t, err, "wizard run")

	assert.True(t, out.ReplaceExisting)
	require.NotNil(t, out.SecretManifest, "new token ⇒ SecretManifest written")
	assert.Equal(t, validBotToken, string(out.SecretManifest.Data[SecretKeyBotToken]), "the new token is stored")
	require.NotNil(t, out.ChannelManifest)
	assert.Equal(t, "slack-monitoring-demo", out.ChannelManifest.Name, "reuse existing name")
	assert.Equal(t, "C0NEWCHAN", out.ChannelManifest.Spec.Slack.OutputDefaults.ChannelID, "the retyped destination")
	assert.Equal(t, "U01NEWBOT", out.ChannelManifest.Spec.Slack.BotUserID, "refreshed BotUserID from auth.test")
}

// TestWizardMonitoring_ReSetup_HonorsExistingCredsSecretName: when the existing
// Channel declares a NON-default CredentialsRef.SecretName (hand-authored /
// pre-declared), re-setup must reuse that name rather than reverting to the
// "<channelName>-creds" convention — otherwise the re-applied Channel points at
// a Secret that holds nothing.
func TestWizardMonitoring_ReSetup_HonorsExistingCredsSecretName(t *testing.T) {
	cases := []struct {
		name       string
		answers    map[string]string
		wizard     *slackWizard
		wantSecret bool
	}{
		{
			name: "keeping the token: the ref still names the pre-declared Secret",
			answers: map[string]string{
				keyHasSlackApp: string(routeHave), keyBotToken: "", keyDestinationChannelID: "C0OLDCHAN",
			},
			wizard: newWizardWithStub(nil, assertNotCalledErr),
		},
		{
			name: "replacing the token: the written Secret uses the pre-declared name",
			answers: map[string]string{
				keyHasSlackApp: string(routeHave), keyBotToken: validBotToken, keyDestinationChannelID: "C0NEWCHAN",
			},
			wizard:     newWizardWithStub(okAuth(), nil),
			wantSecret: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := resolvedMonitoringRun(t, tc.wizard,
				reSetupInput(existingMonitoringChannel("custom-mon-creds"), true), tc.answers)
			require.NoError(t, err, "wizard run")

			require.NotNil(t, out.ChannelManifest)
			assert.Equal(t, "custom-mon-creds", out.ChannelManifest.Spec.CredentialsRef.SecretName,
				"must honor the existing creds Secret name")
			if !tc.wantSecret {
				assert.Nil(t, out.SecretManifest, "kept token ⇒ nil SecretManifest")
				return
			}
			require.NotNil(t, out.SecretManifest, "new token ⇒ SecretManifest written")
			assert.Equal(t, "custom-mon-creds", out.SecretManifest.Name,
				"the written Secret name must match the honored ref")
		})
	}
}

// TestWizardMonitoring_BlankTokenWithNothingStored_Refuses: keep-on-blank is
// only an answer when there is something to keep. Without a stored Secret, a
// blank token is a missing token and must not yield a Channel whose
// credentials Secret was never written.
func TestWizardMonitoring_BlankTokenWithNothingStored_Refuses(t *testing.T) {
	cases := []struct {
		name  string
		input channelkinds.WizardInput
	}{
		{
			name:  "first-time setup: a blank token is refused",
			input: monitoringWizardInput(),
		},
		{
			name:  "re-setup with no stored Secret: a blank token is still refused",
			input: reSetupInput(existingMonitoringChannel("slack-monitoring-demo-creds"), false),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := resolvedMonitoringRun(t, newWizardWithStub(okAuth(), nil), tc.input,
				map[string]string{
					keyHasSlackApp:            string(routeHave),
					keyBotToken:               "",
					keyDestinationChannelID:   "C0123ABCDEF",
					wizardkeys.KeyChannelName: "my-mon",
				})

			require.Error(t, err, "a blank token with nothing stored must stop the run")
			assert.Contains(t, err.Error(), botTokenPrefix, "the error must say what was expected")
			assert.Nil(t, out.ChannelManifest, "no Channel may be produced")
			assert.Nil(t, out.SecretManifest, "no Secret may be produced")
		})
	}
}

// TestWizardMonitoring_RejectedByAuthTest_Refuses: a token Slack does not
// accept must stop the run rather than produce a Channel whose BotUserID is
// blank — a monitoring channel with no bot identity posts as nobody.
func TestWizardMonitoring_RejectedByAuthTest_Refuses(t *testing.T) {
	w := newWizardAcceptingToken("xoxb-some-other-token", okAuth())
	stale := "xoxb-0000000000000-0000000000000-staletokenmaterialxx"

	answers := monitoringAnswers("my-mon")
	answers[keyBotToken] = stale

	out, err := resolvedMonitoringRun(t, w, monitoringWizardInput(), answers)
	require.Error(t, err, "a token Slack rejects must stop the run")
	assert.Contains(t, err.Error(), "auth.test", "the step that refused must be named")
	assert.Nil(t, out.ChannelManifest, "no Channel may be produced")
	assert.Nil(t, out.SecretManifest, "no Secret may be produced")
}

// TestWizardMonitoring_PreflightErrors covers the refusals that happen before
// any question is asked.
func TestWizardMonitoring_PreflightErrors(t *testing.T) {
	cases := []struct {
		name      string
		input     channelkinds.WizardInput
		errSubstr string
	}{
		{
			name:      "no namespace: error mentions namespace",
			input:     channelkinds.WizardInput{Monitoring: true},
			errSubstr: "namespace",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			qs, err := kindWizard(t).Inputs(context.Background(), tc.input)
			require.Error(t, err, "expected a refusal")
			assert.Contains(t, err.Error(), tc.errSubstr, "error message")
			assert.Nil(t, qs, "a refused Inputs declares no questions")
		})
	}
}

// TestWizardMonitoring_NoWiredSlackClient refuses rather than dereferencing a
// nil factory mid-run: Resolve is the only step that talks to Slack, so it is
// the one that has to notice.
func TestWizardMonitoring_NoWiredSlackClient(t *testing.T) {
	_, err := (&slackWizard{}).Resolve(context.Background(), monitoringWizardInput(),
		monitoringAnswers("my-mon"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Slack client factory")
}

// TestWizardMonitoring_ResultRefusesAnUnansweredQuestion: a missing answer must
// not yield a half-built manifest.
func TestWizardMonitoring_ResultRefusesAnUnansweredQuestion(t *testing.T) {
	full := func() map[string]string {
		return map[string]string{
			keyBotToken:               validBotToken,
			keyDestinationChannelID:   "C0123ABCDEF",
			wizardkeys.KeyChannelName: "my-mon",
			keyBotUserID:              "U0DEMO123",
		}
	}
	for missing := range full() {
		t.Run("missing "+missing+": Result refuses and names it", func(t *testing.T) {
			answers := full()
			answers[missing] = ""

			_, err := kindWizard(t).Result(monitoringWizardInput(), answers)
			require.Error(t, err, "Result must refuse an unanswered run")
			assert.Contains(t, err.Error(), missing, "the error must name the missing answer")
		})
	}
}

// TestWizardMonitoring_ResultIsMonitoringOnlyWhenTheInputSaysSo guards the one
// fact that decides which flow Result builds for. It comes from the ARGUMENT
// on every call — never from a field an earlier call set — so the same wizard
// value handed an agent-flow WizardInput must build the agent flow's manifests
// and refuse a monitoring answer set for lacking an AgentClass.
func TestWizardMonitoring_ResultIsMonitoringOnlyWhenTheInputSaysSo(t *testing.T) {
	w := kindWizard(t)

	monitoring, err := w.Result(monitoringWizardInput(), map[string]string{
		keyBotToken:               validBotToken,
		keyDestinationChannelID:   "C0123ABCDEF",
		wizardkeys.KeyChannelName: "my-mon",
		keyBotUserID:              "U0DEMO123",
	})
	require.NoError(t, err, "a monitoring in must build the monitoring flow")
	require.NotNil(t, monitoring.ChannelManifest)
	assert.Equal(t, spiceboxv1alpha1.ChannelRoleMonitoring, monitoring.ChannelManifest.Spec.Role)

	_, err = w.Result(channelkinds.WizardInput{Namespace: "default"}, map[string]string{
		keyBotToken:               validBotToken,
		keyDestinationChannelID:   "C0123ABCDEF",
		wizardkeys.KeyChannelName: "my-mon",
		keyBotUserID:              "U0DEMO123",
	})
	require.Error(t, err, "the same answers under an agent-flow in must build the agent flow")
	assert.Contains(t, err.Error(), keyAgentClass, "it refuses on the answer the agent flow needs")
}
