package slack

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/kindtest"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/x/credmask"
)

// kindWizard is this kind's wizard, reached the way `oap channel create`
// reaches it — through the registered Kind rather than by constructing the
// concrete type — so a Kind.Wizard that stopped returning it is caught here.
//
// It carries the PRODUCTION clients, so it is only for the calls that make no
// network request: Inputs and Result. A Resolve case uses stubWizard.
func kindWizard(t *testing.T) channelkinds.Wizard {
	t.Helper()
	w := (&Kind{}).Wizard()
	require.NotNil(t, w, "the slack kind must return a wizard")
	return w
}

// stubWizard is kindWizard with the live clients replaced, for the Resolve
// cases: the real Kind.Wizard wires a slack-go client and an HTTP
// app-provisioning client, and a test that drove either would reach the
// network.
//
// Nothing here has to suppress the app-manifest write: WizardInput.WorkingDir
// defaults to empty, which means "this client offered nowhere to write", so a
// test drops no file unless it asks for one (see writableInput).
func stubWizard(t *testing.T, w *slackWizard) channelkinds.Wizard {
	t.Helper()
	var dw channelkinds.Wizard = w
	return dw
}

// newWizardAcceptingToken builds a wizard whose auth.test succeeds for exactly
// one bot token and fails for every other — the shape that distinguishes "the
// run verified the token it was given" from "the run accepted whatever it was
// handed".
func newWizardAcceptingToken(good string, resp *slackapi.AuthTestResponse) *slackWizard {
	return &slackWizard{authFactory: func(tok string) authTester {
		if tok == good {
			return &stubAuthTester{resp: resp}
		}
		return &stubAuthTester{err: errors.New("invalid_auth")}
	}}
}

// writableInput is in with a temp directory the run may leave a file in, for
// the cases that assert on the file rather than on the message.
func writableInput(t *testing.T, in channelkinds.WizardInput) channelkinds.WizardInput {
	t.Helper()
	in.WorkingDir = t.TempDir()
	return in
}

// seededAnswers is a WizardInput.Seeded carrying the given answers, in the
// same shape `oap channel create` builds one (channelwizard.Seed).
//
// A COPY, not the literal itself: WizardInput is passed by value but the map
// inside it is not, so a test that went on to mutate its own literal would
// change what a wizard it had already called was given.
func seededAnswers(kv map[string]string) map[string]string {
	out := make(map[string]string, len(kv))
	maps.Copy(out, kv)
	return out
}

// answeredAgentRun is the answer map a completed agent-flow run hands Result:
// the six questions plus the three values Resolve derived from auth.test.
//
// The capability answer is the PRE-CHECKED set an AgentClass with no explicit
// grants starts out as, because that is what a bare-accept screen run records
// — which is what makes this answer set equivalent to happyScript's, and so
// what makes TestWizardResult_MatchesTheScreenPath a comparison of the two
// contracts rather than of two different runs.
func answeredAgentRun(t *testing.T, channelName string) map[string]string {
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
		keyBotUserID:              "U0DEMO123",
		keyTeamName:               "Demo Workspace",
		keyBotUserName:            "demo-bot",
	}
}

// renderedSummary is every label and value this run's summary carries, joined
// — a stand-in for the block the client leaves behind in plain scrollback, and
// what a leaked credential would outlive the run in.
//
// Joined here rather than run through the client's real renderer, because that
// renderer lives in pkg/cli/tui and NOTHING under channelkinds may reach for a
// terminal package, tests included (see channelkinds.Wizard). The
// substitution is safe for the claim being made: the client is documented to
// filter nothing (channelkinds.SummaryNote), so what reaches the terminal is
// exactly these values — and "the client must not filter" is pkg/cli/tui's
// property to hold, not this kind's.
func renderedSummary(t *testing.T, notes []channelkinds.SummaryNote) string {
	t.Helper()
	var b strings.Builder
	for _, n := range notes {
		b.WriteString(n.Label)
		b.WriteString("  ")
		b.WriteString(n.Value)
		b.WriteString("\n")
	}
	return b.String()
}

// summaryValue returns the value recorded under label, and whether it was
// there at all.
func summaryValue(notes []channelkinds.SummaryNote, label string) (string, bool) {
	for _, n := range notes {
		if n.Label == label {
			return n.Value, true
		}
	}
	return "", false
}

// --- Inputs: the prompt-text gate (P5-R3) ---

// pinnedAgentQuestions is what Inputs must return for the agent flow in a
// namespace holding exactly one AgentClass, on a run whose FLAGS have settled
// the route to "I already have an app".
//
// Every prompt is a LITERAL, never a reference to the constant the production
// code uses: comparing a constant against itself proves nothing and would
// silently void this gate.
//
// Nothing here carries an AskWhen. A settled route declares one pair of
// credential questions and no other, so there is no branch left for a gate to
// express — which is exactly what pinnedUndecidedAgentQuestions differs on.
func pinnedAgentQuestions() []kindtest.PromptShape {
	return []kindtest.PromptShape{
		{Name: "agentclass", Type: oap.QEnum, Prompt: "Bind this Slack app to AgentClass", Required: true},
		{Name: "slackapp", Type: oap.QEnum, Prompt: "Have you already created a Slack app for this agent?", Required: true},
		{Name: "capabilities", Type: oap.QResourceList, Prompt: "Enable for this agent", Required: true},
		{Name: "bot-token", Type: oap.QSecret, Prompt: "Bot User OAuth Token (xoxb-)", Required: true},
		{Name: "app-token", Type: oap.QSecret, Prompt: "App-Level Token (xapp-)", Required: true},
		{Name: "name", Type: oap.QString, Prompt: "Channel resource name", Required: true},
	}
}

// pinnedUndecidedAgentQuestions is what Inputs must return when NO flag has
// settled the route — which is every interactive run, and the shape the
// provisioning route was unreachable in.
//
// THE ASKWHEN VALUES ARE THE PAYLOAD of this fixture, not decoration. The set
// declares both pairs of credential questions so that every key stays seedable,
// and the gates are the only thing deciding which pair the operator meets: drop
// them and each row still pins its Name, Type, Prompt and Required exactly as
// written here while all four questions are put to every operator — which is
// the defect, restored, with this gate still green.
//
// The gate values are the STORED route spellings (`true`, `provision`), the
// ones `--answer slackapp=` already depends on, rather than the labels the
// operator reads.
func pinnedUndecidedAgentQuestions() []kindtest.PromptShape {
	haveAnApp := oap.AskWhen{Question: "slackapp", In: []string{"true"}}
	provisioning := oap.AskWhen{Question: "slackapp", In: []string{"provision"}}
	return []kindtest.PromptShape{
		{Name: "agentclass", Type: oap.QEnum, Prompt: "Bind this Slack app to AgentClass", Required: true},
		{Name: "slackapp", Type: oap.QEnum, Prompt: "Have you already created a Slack app for this agent?", Required: true},
		{Name: "capabilities", Type: oap.QResourceList, Prompt: "Enable for this agent", Required: true},
		{Name: "bot-token", Type: oap.QSecret, Prompt: "Bot User OAuth Token (xoxb-)", Required: true, AskWhen: haveAnApp},
		{Name: "app-token", Type: oap.QSecret, Prompt: "App-Level Token (xapp-)", Required: true, AskWhen: haveAnApp},
		{Name: "app-token-source", Type: oap.QEnum, Prompt: "How should oap authenticate with Slack?", Required: true, AskWhen: provisioning},
		// Optional because a source that mints its own token ignores the
		// value, and which source that will be is not known when this batch is
		// declared.
		{Name: "app-config-token", Type: oap.QSecret, Prompt: "App configuration token", Required: false, AskWhen: provisioning},
		{Name: "name", Type: oap.QString, Prompt: "Channel resource name", Required: true},
	}
}

// TestWizardInputs_MatchesPinnedPromptShapes is slack's per-kind gate on
// P5-R3: the CLI's pinned-prompts test records only AnswerKeys, which is
// hand-maintained on each screen and never derived from what Prepare actually
// builds — a question that lost its widget while keeping its declared key
// would be invisible there. PROMPT TEXT is not determined by AnswerKeys, so
// asserting it here is the independent check.
//
// kindtest.AssertPromptShapes both validates (channelkinds.ValidateInputs) and
// compares the whole slice in one assert.Equal, so an ADDED, DROPPED or
// REORDERED question fails exactly as loudly as a reworded prompt.
func TestWizardInputs_MatchesPinnedPromptShapes(t *testing.T) {
	qs, err := kindWizard(t).Inputs(context.Background(),
		defaultInput(newAgentClass("demo-agent", "default")))
	require.NoError(t, err)

	kindtest.AssertPromptShapes(t, qs, pinnedUndecidedAgentQuestions())
}

// TestWizardInputs_UndecidedRouteAsksEachRouteOnlyItsOwnCredentials is what
// the pinned shape above cannot say on its own: that the gates it records
// actually SELECT, and select the right pair each way.
//
// It reads the declared set the way a client does — oap.Question.Applies
// against the answers so far — for each of the three routes an operator can
// pick at the prompt. The provisioning row is the one that was unreachable:
// an operator who chose it was asked for the two tokens it exists to mint and
// never asked how to authenticate with the API that mints them.
func TestWizardInputs_UndecidedRouteAsksEachRouteOnlyItsOwnCredentials(t *testing.T) {
	qs, err := kindWizard(t).Inputs(context.Background(),
		defaultInput(newAgentClass("demo-agent", "default")))
	require.NoError(t, err)

	cases := []struct {
		name  string
		route appRoute
		want  []string
	}{
		{
			name:  "provision: asked how to authenticate, never for the tokens it mints",
			route: routeProvision,
			want:  []string{keyAgentClass, keyHasSlackApp, keyCapabilities, keyTokenSource, keyConfigToken, wizardkeys.KeyChannelName},
		},
		{
			name:  "have an app: asked for the two tokens, never how to authenticate",
			route: routeHave,
			want:  []string{keyAgentClass, keyHasSlackApp, keyCapabilities, keyBotToken, keyAppToken, wizardkeys.KeyChannelName},
		},
		{
			name:  "manual: asked for no credential at all, the run ending with the manifest",
			route: routeManual,
			want:  []string{keyAgentClass, keyHasSlackApp, keyCapabilities, wizardkeys.KeyChannelName},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			answers := map[string]string{keyHasSlackApp: string(tc.route)}
			var asked []string
			for _, q := range qs {
				if q.Applies(func(k string) string { return answers[k] }) {
					asked = append(asked, q.Name)
				}
			}
			assert.Equal(t, tc.want, asked)
		})
	}
}

// TestWizardInputs_SettledRouteMakesTheTokensRequired is the flag-settled
// shape: once a flag has said the app exists, the two tokens are the only way
// this run can finish, so a blank one is a mistake the widget should refuse in
// place rather than something Resolve discovers later — and no other route's
// questions are declared at all, which is what lets checkAnswerKeys refuse a
// stray provisioning key beside it.
func TestWizardInputs_SettledRouteMakesTheTokensRequired(t *testing.T) {
	in := defaultInput(newAgentClass("demo-agent", "default"))
	in.Seeded = seededAnswers(map[string]string{keyHasSlackApp: string(routeHave)})

	qs, err := kindWizard(t).Inputs(context.Background(), in)
	require.NoError(t, err)

	kindtest.AssertPromptShapes(t, qs, pinnedAgentQuestions())
}

// TestWizardInputs_ManualRouteAsksOnlyWhatTheManifestNeeds is the fix for the
// shape that made an operator invent two credentials in order to be shown a
// manifest: once a flag has chosen the manual route, this run cannot end in a
// Channel, so it asks for the AgentClass and the capabilities the manifest is
// generated from and nothing else.
func TestWizardInputs_ManualRouteAsksOnlyWhatTheManifestNeeds(t *testing.T) {
	in := defaultInput(newAgentClass("demo-agent", "default"))
	in.Seeded = seededAnswers(map[string]string{keyHasSlackApp: string(routeManual)})

	qs, err := kindWizard(t).Inputs(context.Background(), in)
	require.NoError(t, err)

	kindtest.AssertPromptShapes(t, qs, []kindtest.PromptShape{
		{Name: "agentclass", Type: oap.QEnum, Prompt: "Bind this Slack app to AgentClass", Required: true},
		{Name: "slackapp", Type: oap.QEnum, Prompt: "Have you already created a Slack app for this agent?", Required: true},
		{Name: "capabilities", Type: oap.QResourceList, Prompt: "Enable for this agent", Required: true},
	})
}

// TestWizardInputs_ProvisioningRouteSwapsTheTokensForTheConfigQuestions pins
// the one route that changes the question set, and pins it against the
// condition that decides it: the two configuration keys are declared exactly
// when the two token keys are not, so the set a caller may seed never
// includes a key this run cannot use.
func TestWizardInputs_ProvisioningRouteSwapsTheTokensForTheConfigQuestions(t *testing.T) {
	in := defaultInput(newAgentClass("demo-agent", "default"))
	in.Seeded = seededAnswers(map[string]string{keyHasSlackApp: string(routeProvision)})

	qs, err := kindWizard(t).Inputs(context.Background(), in)
	require.NoError(t, err)

	kindtest.AssertPromptShapes(t, qs, []kindtest.PromptShape{
		{Name: "agentclass", Type: oap.QEnum, Prompt: "Bind this Slack app to AgentClass", Required: true},
		{Name: "slackapp", Type: oap.QEnum, Prompt: "Have you already created a Slack app for this agent?", Required: true},
		{Name: "capabilities", Type: oap.QResourceList, Prompt: "Enable for this agent", Required: true},
		{Name: "app-token-source", Type: oap.QEnum, Prompt: "How should oap authenticate with Slack?", Required: true},
		// Optional because a source that mints its own token ignores the
		// value, and which source that will be is not known when this batch is
		// declared.
		{Name: "app-config-token", Type: oap.QSecret, Prompt: "App configuration token", Required: false},
		{Name: "name", Type: oap.QString, Prompt: "Channel resource name", Required: true},
	})
}

// TestWizardInputs_ProvisioningRouteWithBothTokensSeededAsksForNeither mirrors
// tokensScreen.AnswerKeys' tokensSeeded half: a caller who provisioned once,
// saved both tokens and re-runs with slackapp=provision still set is not
// asking to provision again.
func TestWizardInputs_ProvisioningRouteWithBothTokensSeededAsksForNeither(t *testing.T) {
	in := defaultInput(newAgentClass("demo-agent", "default"))
	in.Seeded = seededAnswers(map[string]string{
		keyHasSlackApp: string(routeProvision),
		keyBotToken:    validBotToken,
		keyAppToken:    validAppToken,
	})

	qs, err := kindWizard(t).Inputs(context.Background(), in)
	require.NoError(t, err)

	var names []string
	for _, q := range qs {
		names = append(names, q.Name)
	}
	assert.NotContains(t, names, keyTokenSource,
		"a run with both tokens in hand has nothing to provision, so nothing to authenticate for")
	assert.NotContains(t, names, keyConfigToken)
	assert.Contains(t, names, keyBotToken, "the tokens are the questions again, even seeded")
	assert.Contains(t, names, keyAppToken)
}

// pinnedMonitoringQuestions is what Inputs must return for a first-time
// monitoring run.
// settled says whether a flag already chose a route this flow can serve. On an
// UNDECIDED run everything after the Slack-app question is optional: the
// operator may answer it with "no app yet", and this flow's response to that
// is to end the run — which they cannot reach if a required question with no
// default refuses their blank first. See monitoringFlow.inputs.
func pinnedMonitoringQuestions(settled bool) []kindtest.PromptShape {
	return []kindtest.PromptShape{
		{Name: "slackapp", Type: oap.QEnum, Prompt: "Have you already created a Slack app with a bot token?", Required: true},
		{Name: "bot-token", Type: oap.QSecret, Prompt: "Bot User OAuth Token (xoxb-)", Required: settled},
		{Name: "destination", Type: oap.QString, Prompt: "Slack channel ID", Required: settled},
		{Name: "name", Type: oap.QString, Prompt: "Channel resource name", Required: settled},
	}
}

// TestWizardInputs_MonitoringMatchesPinnedPromptShapes is the same P5-R3 gate
// for the monitoring flow, which the CLI's pinned-prompts capture does not
// cover at all — it drives Screens with Monitoring unset.
func TestWizardInputs_MonitoringMatchesPinnedPromptShapes(t *testing.T) {
	qs, err := kindWizard(t).Inputs(context.Background(), monitoringWizardInput())
	require.NoError(t, err)

	kindtest.AssertPromptShapes(t, qs, pinnedMonitoringQuestions(false))
}

// TestWizardInputs_MonitoringSettledRouteMakesTheRestRequired is the other
// half: once a flag has said the app exists, every remaining answer is one
// this run needs, so a blank is refused in place.
func TestWizardInputs_MonitoringSettledRouteMakesTheRestRequired(t *testing.T) {
	in := monitoringWizardInput()
	in.Seeded = seededAnswers(map[string]string{keyHasSlackApp: string(routeHave)})

	qs, err := kindWizard(t).Inputs(context.Background(), in)
	require.NoError(t, err)

	kindtest.AssertPromptShapes(t, qs, pinnedMonitoringQuestions(true))
}

// TestWizardInputs_MonitoringRefusesASeededRouteItCannotServe moves
// monitoringSetupScreen's two refusals to the only moment that beats the
// questions to it. Both messages are static — neither is derived from an
// answer — so a run whose flags already chose an unservable route has nothing
// to collect first, and asking for a bot token, a destination and a Channel
// name before saying "you have no Slack app yet" is exactly the ordering that
// screen exists to avoid. This is `oap init`'s path.
func TestWizardInputs_MonitoringRefusesASeededRouteItCannotServe(t *testing.T) {
	cases := []struct {
		name      string
		route     appRoute
		errSubstr string
	}{
		{
			name:      "no app yet: the create-an-app steps, before any question",
			route:     routeManual,
			errSubstr: "no Slack app with a bot token yet",
		},
		{
			name:      "provision: refused, because nothing in this flow can create an app",
			route:     routeProvision,
			errSubstr: "automatic Slack app creation is not offered",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := monitoringWizardInput()
			in.Seeded = seededAnswers(map[string]string{keyHasSlackApp: string(tc.route)})

			qs, err := kindWizard(t).Inputs(context.Background(), in)
			require.Error(t, err)
			assert.Nil(t, qs, "a refused flow declares no questions")
			assert.Contains(t, err.Error(), tc.errSubstr)
		})
	}
}

// TestWizardInputs_MonitoringResetupDropsTheNameAndAcceptsABlankToken pins
// both halves of re-setup, matching monitoringNameScreen.AnswerKeys (nothing
// on re-setup) and monitoringTokenScreen's keep-on-blank.
func TestWizardInputs_MonitoringResetupDropsTheNameAndAcceptsABlankToken(t *testing.T) {
	qs, err := kindWizard(t).Inputs(context.Background(),
		reSetupInput(existingMonitoringChannel("slack-monitoring-demo-creds"), true))
	require.NoError(t, err)

	kindtest.AssertPromptShapes(t, qs, []kindtest.PromptShape{
		{Name: "slackapp", Type: oap.QEnum, Prompt: "Have you already created a Slack app with a bot token?", Required: true},
		// Optional twice over here: a blank answer means "keep the token
		// already stored", AND no flag has settled the route yet.
		{Name: "bot-token", Type: oap.QSecret, Prompt: "Bot User OAuth Token (xoxb-)", Required: false},
		{Name: "destination", Type: oap.QString, Prompt: "Slack channel ID", Required: false},
	})

	byName := map[string]oap.Question{}
	for _, q := range qs {
		byName[q.Name] = q
	}
	assert.Contains(t, byName[keyBotToken].Description, "Leave it blank to keep the token already stored",
		"the keep-on-blank line is the only thing telling the operator what an empty answer means")
	assert.Equal(t, "C0OLDCHAN", byName[keyDestinationChannelID].Default,
		"re-setup offers the destination already configured rather than making the user look it up again")
}

// TestWizardInputs_MonitoringResetupRefusesADifferentName is
// monitoringNameScreen.Apply's refusal, which has to move somewhere: the name
// question is not declared on re-setup, so a seeded name would otherwise be
// read by nothing at all — silently, since `--name` reaches WizardInput.Seeded
// without being an `--answer` the declared-key check ever sees.
func TestWizardInputs_MonitoringResetupRefusesADifferentName(t *testing.T) {
	in := reSetupInput(existingMonitoringChannel("slack-monitoring-demo-creds"), true)
	in.Seeded = seededAnswers(map[string]string{wizardkeys.KeyChannelName: "some-other-name"})

	_, err := kindWizard(t).Inputs(context.Background(), in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "slack-monitoring-demo")
	assert.Contains(t, err.Error(), "some-other-name")
}

// TestWizardInputs_TokensAreSecretTyped: QSecret is what makes every client
// treat these as credentials — echo off in a form, no value in a log.
func TestWizardInputs_TokensAreSecretTyped(t *testing.T) {
	qs, err := kindWizard(t).Inputs(context.Background(),
		defaultInput(newAgentClass("demo-agent", "default")))
	require.NoError(t, err)

	byName := map[string]oap.Question{}
	for _, q := range qs {
		byName[q.Name] = q
	}
	for _, name := range []string{keyBotToken, keyAppToken} {
		require.Contains(t, byName, name)
		assert.Equal(t, oap.QSecret, byName[name].Type,
			"%s is a credential: QSecret is what makes every client mask it", name)
	}
}

// TestWizardInputs_KeepsTheDerivedChannelNameWithASingleAgentClass pins one
// half of P5-R16: with exactly one AgentClass in the namespace, classes[0] IS
// the unambiguous answer, so the Channel name keeps deriving from it —
// matching what a bare-accept Screens run also produces.
func TestWizardInputs_KeepsTheDerivedChannelNameWithASingleAgentClass(t *testing.T) {
	qs, err := kindWizard(t).Inputs(context.Background(),
		defaultInput(newAgentClass("demo-agent", "default")))
	require.NoError(t, err)

	byName := map[string]oap.Question{}
	for _, q := range qs {
		byName[q.Name] = q
	}
	assert.Equal(t, "demo-agent", byName[keyAgentClass].Default, "AgentClass default")
	assert.Equal(t, "slack-demo-agent", byName[wizardkeys.KeyChannelName].Default, "Name default")
}

// TestWizardInputs_OmitsTheDerivedChannelNameWithMultipleAgentClasses pins the
// other half of P5-R16 (a wrong default is worse than no default): deriving
// the Channel name from classes[0] would suggest a name built from an
// AgentClass the operator may not have meant to pick — silently, since a blank
// answer just accepts it.
//
// The AgentClass question's OWN default is unaffected: an enum's widget
// pre-selects the first option regardless, so there is no "omit" available for
// an enum the way there is for free text.
func TestWizardInputs_OmitsTheDerivedChannelNameWithMultipleAgentClasses(t *testing.T) {
	qs, err := kindWizard(t).Inputs(context.Background(), defaultInput(
		newAgentClass("alpha-agent", "default"),
		newAgentClass("zeta-agent", "default"),
	))
	require.NoError(t, err)

	byName := map[string]oap.Question{}
	for _, q := range qs {
		byName[q.Name] = q
	}
	assert.Equal(t, "alpha-agent", byName[keyAgentClass].Default,
		"the AgentClass picker's own pre-selection is unaffected by this ruling")
	assert.Nil(t, byName[wizardkeys.KeyChannelName].Default,
		"Name must have NO derived default with more than one AgentClass in the namespace")
}

// newAgentClassDisabling builds an AgentClass that has explicitly turned one
// default-on capability OFF, which is what makes it distinguishable from the
// registry defaults.
func newAgentClassDisabling(name, namespace, capability string) *spiceboxv1alpha1.AgentClass {
	return newAgentClassWithCaps(name, namespace, map[string]string{
		capability: `{"enabled":false}`,
	})
}

// TestWizardInputs_CapabilityPrecheckIgnoresAGuessedAgentClass is the
// regression test for the worst thing a single up-front batch could do, and
// the reason capabilityQuestion is handed "" rather than the AgentClass
// question's own Default.
//
// With more than one AgentClass in the namespace, the enum pre-selects
// classes[0] — a GUESS, not an answer. Pre-checking the capability boxes from
// THAT class's grants and then applying them to whichever class the operator
// actually picks would revoke, on the picked class, a capability it never
// disabled: capabilityPatch writes an explicit enabled:false for every offered
// capability, so a bare accept is a full rewrite, not a no-op — and Inputs
// runs before any class answer exists, so the pre-check has nothing better
// than the registry defaults to fall back to.
//
// The fixture is what discriminates: alpha-agent (classes[0], alphabetically
// first) disables a default-on capability, so a pre-check taken from it is
// visibly SHORTER than the registry defaults. Asserting equality with the
// defaults is therefore an assertion that the guess was not consulted.
func TestWizardInputs_CapabilityPrecheckIgnoresAGuessedAgentClass(t *testing.T) {
	const disabled = "thread_history"
	defaults, err := activeCapabilities(slackCapabilityOptions(), nil)
	require.NoError(t, err)
	require.Contains(t, defaults, disabled,
		"the fixture only discriminates if the capability it disables is on by default")

	qs, err := kindWizard(t).Inputs(context.Background(), defaultInput(
		newAgentClassDisabling("alpha-agent", "default", disabled),
		newAgentClass("zeta-agent", "default"),
	))
	require.NoError(t, err)

	byName := map[string]oap.Question{}
	for _, q := range qs {
		byName[q.Name] = q
	}
	require.Equal(t, "alpha-agent", byName[keyAgentClass].Default,
		"alpha-agent must be the guess this test proves is NOT consulted")
	assert.Equal(t, defaults, byName[keyCapabilities].Default,
		"with the AgentClass still a guess the pre-check must come from the capability "+
			"defaults; taking it from classes[0] would revoke %q on whichever class the "+
			"operator actually picks", disabled)
}

// TestWizardInputs_CapabilityPrecheckReadsAnUnambiguousAgentClass is the other
// half: with exactly one AgentClass in the namespace there is no guess, so the
// pre-check reads what that class actually grants — matching what the screen
// path shows once the same class has been chosen.
func TestWizardInputs_CapabilityPrecheckReadsAnUnambiguousAgentClass(t *testing.T) {
	const disabled = "thread_history"
	qs, err := kindWizard(t).Inputs(context.Background(),
		defaultInput(newAgentClassDisabling("only-agent", "default", disabled)))
	require.NoError(t, err)

	byName := map[string]oap.Question{}
	for _, q := range qs {
		byName[q.Name] = q
	}
	checked, ok := byName[keyCapabilities].Default.([]string)
	require.True(t, ok, "the capability question's Default is the pre-checked set")
	assert.NotContains(t, checked, disabled,
		"the only AgentClass in the namespace is not a guess, so what it has turned off shows unchecked")
}

// TestWizardInputs_EnumsCarryTheLabelsTheOperatorReads is the P5-R20 gate.
// oap.Question.Enum holds the STORED answer — `--answer slackapp=true`,
// `--answer app-token-source=paste`, `--answer capabilities=attachments` are
// all documented spellings that cannot change — so without EnumLabels the
// operator picks between raw values. The capability list is the one that
// matters most: its label is where each checkbox's SCOPE COST is written, and
// that is the thing being consented to.
func TestWizardInputs_EnumsCarryTheLabelsTheOperatorReads(t *testing.T) {
	in := defaultInput(newAgentClass("demo-agent", "default"))
	qs, err := kindWizard(t).Inputs(context.Background(), in)
	require.NoError(t, err)

	byName := map[string]oap.Question{}
	for _, q := range qs {
		byName[q.Name] = q
	}

	// Literals, not routeLabel(...): a label compared against the function that
	// produced it proves nothing.
	assert.Equal(t, []string{"false", "true", "provision"}, byName[keyHasSlackApp].Enum,
		"the stored answers are frozen — --answer slackapp=true already works")
	assert.Equal(t, []string{
		"Show me the manifest — I'll create it myself",
		"I already have a Slack app",
		"Create one for me",
	}, byName[keyHasSlackApp].EnumLabels, "and the rows say what they mean")

	capQ := byName[keyCapabilities]
	require.Len(t, capQ.EnumLabels, len(capQ.Enum),
		"the pairing is positional; a short list would label the wrong rows")
	assert.Contains(t, capQ.EnumLabels, "attachments — files:read, files:write",
		"a capability's label is what tells the operator the scopes it costs")

	monitoringQs, err := kindWizard(t).Inputs(context.Background(), monitoringWizardInput())
	require.NoError(t, err)
	for _, q := range monitoringQs {
		if q.Name == keyHasSlackApp {
			assert.Equal(t, []string{"false", "true"}, q.Enum)
			assert.Equal(t, []string{
				"Show me the manifest — I'll create it myself",
				"I already have a Slack app",
			}, q.EnumLabels, "the monitoring flow's two rows are labelled too")
			return
		}
	}
	t.Fatal("the monitoring flow must ask the Slack-app question")
}

// TestWizardInputs_ProvisioningSourcesCarryTheirOwnLabels: a token source's
// key is what appprovision.SourceFor resolves and what `--answer
// app-token-source=paste` names; its Label() is the sentence the operator
// picks by.
func TestWizardInputs_ProvisioningSourcesCarryTheirOwnLabels(t *testing.T) {
	in := defaultInput(newAgentClass("demo-agent", "default"))
	in.Seeded = seededAnswers(map[string]string{keyHasSlackApp: string(routeProvision)})

	qs, err := kindWizard(t).Inputs(context.Background(), in)
	require.NoError(t, err)

	for _, q := range qs {
		if q.Name != keyTokenSource {
			continue
		}
		require.Len(t, q.EnumLabels, len(q.Enum))
		assert.Contains(t, q.Enum, "paste", "the stored answer stays the source key")
		assert.Contains(t, q.EnumLabels, "Paste a configuration token from api.slack.com",
			"and the row is the source's own label")
		return
	}
	t.Fatal("a provisioning run must ask how to authenticate")
}

// TestWizardInputs_MonitoringNameHasNoDerivedDefault pins P5-R16 on the
// monitoring flow: the only fact worth naming a monitoring Channel after is
// the Slack workspace, which auth.test does not report until Resolve — after
// this batch is declared. A default derived from what is known HERE
// would be a bare prefix, which a bare accept would take silently.
func TestWizardInputs_MonitoringNameHasNoDerivedDefault(t *testing.T) {
	qs, err := kindWizard(t).Inputs(context.Background(), monitoringWizardInput())
	require.NoError(t, err)

	for _, q := range qs {
		if q.Name == wizardkeys.KeyChannelName {
			assert.Nil(t, q.Default,
				"the workspace name this used to derive from is not known when Inputs runs")
			return
		}
	}
	t.Fatal("the monitoring flow must ask for a Channel name on first setup")
}

// TestWizardInputs_RefusesASeededAgentClassThatDoesNotExist keeps
// agentClassScreen.Prepare's refusal, now living in
// wizardkeys.AgentClassQuestion, which is the reason that question lists the
// namespace even for a seeded answer: a flag must not bind a Channel to an
// agent that does not exist.
//
// IT NAMES THE GUARD IT MEANS, because two different guards refuse this input
// and only one of them is the subject. With the seeded-verification check
// removed, Inputs still errors — capabilityQuestion goes on to Get the bound
// class to pre-check the capability boxes, and that Get fails with a message
// carrying BOTH "no-such-agent" and "not found". So the obvious pair of
// substring assertions is satisfied by the exact regression this test exists
// to catch. Asserting the guard's own sentence, and refusing the Get's, is
// what makes it discriminate.
func TestWizardInputs_RefusesASeededAgentClassThatDoesNotExist(t *testing.T) {
	in := defaultInput(newAgentClass("demo-agent", "default"))
	in.Seeded = seededAnswers(map[string]string{keyAgentClass: "no-such-agent"})

	_, err := kindWizard(t).Inputs(context.Background(), in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no-such-agent", "the refusal must name the AgentClass that is missing")
	assert.Contains(t, err.Error(), "cannot bind to an agent that does not exist",
		"and must be the binding guard's own refusal — the only one that runs before anything else looks at the class")
	assert.NotContains(t, err.Error(), "to see what it already grants",
		"reaching capabilityQuestion's Get means the binding guard did not fire; that error mentions the same "+
			"name and 'not found', so a laxer assertion would pass with the guard removed")
}

// TestWizardInputs_RequiresAgentClasses: the refusal
// for a namespace with nothing to bind to.
func TestWizardInputs_RequiresAgentClasses(t *testing.T) {
	_, err := kindWizard(t).Inputs(context.Background(), defaultInput())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no AgentClasses")
}

// TestWizardInputs_RequiresANamespace mirrors Screens' first refusal.
func TestWizardInputs_RequiresANamespace(t *testing.T) {
	_, err := kindWizard(t).Inputs(context.Background(), channelkinds.WizardInput{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "namespace")
}

// TestWizardHandoff_IsNil: slack's one browser step — creating the app by hand
// — is not a callback, so there is nothing for a HandoffSpec to describe.
func TestWizardHandoff_IsNil(t *testing.T) {
	spec, err := kindWizard(t).Handoff(context.Background(), defaultInput())
	require.NoError(t, err)
	assert.Nil(t, spec, "slack receives no callback")
}

// --- Resolve (P5-R17) ---

// TestWizardResolve_RecordsWhoSlackSaysTheTokenBelongsTo is the whole reason
// the contract gained this step: validateScreen's auth.test call cannot be
// skipped, because a Channel with an empty BotUserID cannot recognise its own
// messages, and neither Inputs (which runs before the token exists) nor Result
// (which must stay pure) can make it.
func TestWizardResolve_RecordsWhoSlackSaysTheTokenBelongsTo(t *testing.T) {
	w := stubWizard(t, newWizardWithStub(okAuth(), nil))

	derived, err := w.Resolve(context.Background(), defaultInput(), map[string]string{
		keyHasSlackApp: string(routeHave),
		keyBotToken:    validBotToken,
		keyAppToken:    validAppToken,
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		keyBotUserID:   "U0DEMO123",
		keyTeamName:    "Demo Workspace",
		keyBotUserName: "demo-bot",
	}, derived)
}

// TestWizardResolve_RefusesATokenSlackRejects: a rejected credential is a
// user-facing failure, which is why it is a step of its own rather than
// something buried inside Result.
func TestWizardResolve_RefusesATokenSlackRejects(t *testing.T) {
	w := stubWizard(t, newWizardAcceptingToken("xoxb-some-other-token", okAuth()))

	_, err := w.Resolve(context.Background(), defaultInput(), map[string]string{
		keyHasSlackApp: string(routeHave),
		keyBotToken:    validBotToken,
		keyAppToken:    validAppToken,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth.test")
}

// TestWizardResolve_RefusesATokenPastedIntoTheWrongField keeps
// checkTokenPrefix's recoverable half, which was tokensScreen's field
// validator. channelkinds.ValidateInputs refuses Question.Validation (nothing
// on the channel side evaluates it), so this step is where the check lands —
// and it must run BEFORE the round trip, or Slack's "invalid_auth" is all the
// operator gets for the commonest mistake there is.
func TestWizardResolve_RefusesATokenPastedIntoTheWrongField(t *testing.T) {
	w := stubWizard(t, newWizardWithStub(okAuth(), nil))

	_, err := w.Resolve(context.Background(), defaultInput(), map[string]string{
		keyHasSlackApp: string(routeHave),
		// The two swapped, which is the common mistake: both are copied from
		// the same app.
		keyBotToken: validAppToken,
		keyAppToken: validBotToken,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), botTokenPrefix, "the refusal must name the prefix the field wants")
}

// TestWizardResolve_ProvisionsTheAppWhenTheRouteAsksForOne: the provisioning
// route's create+install is I/O deriving further answers from answers already
// given, which is exactly what this step is for. Without it the route would
// collect a configuration token and then have nothing to do with it.
func TestWizardResolve_ProvisionsTheAppWhenTheRouteAsksForOne(t *testing.T) {
	fake := &fakeProvisionClient{}
	w := stubWizard(t, newWizardWithProvisioner(fake))

	derived, err := w.Resolve(context.Background(), defaultInput(), map[string]string{
		keyAgentClass:   "demo-agent",
		keyHasSlackApp:  string(routeProvision),
		keyCapabilities: strings.Join(optionNames(capabilityOptions(&Kind{})), ","),
		keyTokenSource:  "paste",
		keyConfigToken:  "xoxe.xoxp-demo",
	})
	require.NoError(t, err)

	assert.True(t, fake.createCalled, "the app must actually be created")
	assert.True(t, fake.installCalled, "and installed, or there are no tokens")
	assert.Equal(t, "xoxe.xoxp-demo", fake.gotToken, "the configuration token must reach the client")
	assert.Equal(t, validBotToken, derived[keyBotToken], "the minted bot token is a derived answer")
	assert.Equal(t, validAppToken, derived[keyAppToken])
	assert.Equal(t, "A0DEMO", derived[keyAppID],
		"the app ID is the only record that an app now exists; nothing in Slack's API lists one")
	assert.Equal(t, "U0DEMO123", derived[keyBotUserID], "and the minted token is still verified")
}

// TestWizardResolve_ManualRouteEndsTheRunWithTheManifest is the departure
// slack.manualRouteRefusal documents: a single up-front batch of questions
// cannot hold an operator at a screen while they create an app in a browser,
// so the run ends with the manifest in hand instead of asking for tokens that
// do not exist yet.
func TestWizardResolve_ManualRouteEndsTheRunWithTheManifest(t *testing.T) {
	w := stubWizard(t, newWizardWithStub(okAuth(), nil))

	_, err := w.Resolve(context.Background(), defaultInput(), map[string]string{
		keyAgentClass:   "demo-agent",
		keyHasSlackApp:  string(routeManual),
		keyCapabilities: strings.Join(optionNames(capabilityOptions(&Kind{})), ","),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "api.slack.com", "the refusal must say where the work happens")
	assert.Contains(t, err.Error(), "display_information",
		"and must carry the generated manifest, which is what the operator came here for")
	assert.Contains(t, err.Error(), "demo-agent-bot",
		"generated for THIS agent, not a placeholder")
}

// TestWizardResolve_ManualRouteLeavesTheManifestOnDisk is the half of that
// route the message cannot carry. The refusal is delivered as an error, so it
// lands in scrollback the next command scrolls away and a `2>/dev/null` never
// sees at all; the copy beside the run is what the operator still has when
// they come back to create the app — which is what makes the two-run shape
// tolerable at all.
func TestWizardResolve_ManualRouteLeavesTheManifestOnDisk(t *testing.T) {
	w := stubWizard(t, newWizardWithStub(okAuth(), nil))
	in := writableInput(t, defaultInput())

	_, err := w.Resolve(context.Background(), in, map[string]string{
		keyAgentClass:   "demo-agent",
		keyHasSlackApp:  string(routeManual),
		keyCapabilities: strings.Join(optionNames(capabilityOptions(&Kind{})), ","),
	})
	require.Error(t, err)

	path := filepath.Join(in.WorkingDir, manifestFileName("demo-agent"))
	onDisk, readErr := os.ReadFile(path)
	require.NoError(t, readErr, "the manifest must be written beside the run, not only into the message")
	assert.Contains(t, string(onDisk), "display_information")
	assert.Contains(t, string(onDisk), "demo-agent-bot",
		"the file is the manifest for THIS agent, generated from this run's answers")
	assert.Contains(t, err.Error(), path,
		"and the message must say where it landed, or the operator cannot find it")
}

// TestWizardResolve_ManualRouteWritesNothingWithoutAWorkingDir is the other
// half of WizardInput.WorkingDir's default, and the reason that default is
// empty: a client that offered nowhere to write — a server rendering this
// wizard — must not have the kind pick a directory on its behalf. The manifest
// is still delivered, in the message, and the "A copy is saved at" block is
// simply absent rather than claiming a file that is not there.
func TestWizardResolve_ManualRouteWritesNothingWithoutAWorkingDir(t *testing.T) {
	// Cheap belt-and-braces: if the code ignored WorkingDir and fell back to
	// the process working directory, the file would land HERE rather than in
	// the package directory, and the assertion below still catches it.
	guard := t.TempDir()
	t.Chdir(guard)

	w := stubWizard(t, newWizardWithStub(okAuth(), nil))
	in := defaultInput()
	require.Empty(t, in.WorkingDir, "the zero WizardInput must offer no writable directory")

	_, err := w.Resolve(context.Background(), in, map[string]string{
		keyAgentClass:   "demo-agent",
		keyHasSlackApp:  string(routeManual),
		keyCapabilities: strings.Join(optionNames(capabilityOptions(&Kind{})), ","),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "display_information",
		"the manifest is still delivered; only the copy on disk is withheld")
	assert.NotContains(t, err.Error(), "A copy is saved at",
		"nothing was written, so the message must not claim a file")

	entries, readErr := os.ReadDir(guard)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "no file may be written when the client offered nowhere to write")
}

// TestWizardResolve_ManualRouteMessageSpellsOutTheSecondRun: the run has ended
// and nothing was applied, so the second run starts from nothing. Naming a
// flag and leaving the operator to reconstruct the rest is how they end up
// picking different capabilities the second time — which means an app whose
// scopes no longer match the Channel that uses it.
func TestWizardResolve_ManualRouteMessageSpellsOutTheSecondRun(t *testing.T) {
	w := stubWizard(t, newWizardWithStub(okAuth(), nil))

	_, err := w.Resolve(context.Background(), defaultInput(), map[string]string{
		keyAgentClass:             "demo-agent",
		keyHasSlackApp:            string(routeManual),
		keyCapabilities:           "attachments,thread_history",
		wizardkeys.KeyChannelName: "demo-channel",
	})
	require.Error(t, err)

	msg := err.Error()
	assert.Contains(t, msg, "oap channel create --kind slack", "the command itself, not just a flag")
	assert.Contains(t, msg, "--answer slackapp=true")
	assert.Contains(t, msg, "--answer agentclass=demo-agent",
		"the answers this run already gave are carried into the command, because nothing else carries them")
	assert.Contains(t, msg, "--answer capabilities=attachments,thread_history")
	assert.Contains(t, msg, "--name demo-channel")
	// The namespace is as much a part of the second run as the answers are: a
	// pasted command without it creates the Channel where the agent is not.
	// `oap agent install`'s own finishing command spells it the same way.
	assert.Contains(t, msg, "--namespace "+defaultInput().Namespace,
		"the second run must land in the namespace this one was aimed at")
	assert.NotContains(t, msg, "come back:\nthen ",
		"the lead-in already says 'then come back'; the line after it must not start with another 'then'")
}

// TestWizardResolve_ManualRouteCarriesADeliberatelyEmptyCapabilityAnswer is
// the case a plain "is the joined string non-empty?" check loses, and it loses
// it in the permissive direction.
//
// An operator who unchecked every capability has answered the question. Told
// only to re-run with the AgentClass, their second run re-defaults to every
// default-on capability — so the app's manifest requests minimal scopes while
// the Channel declares more, which is precisely the drift dataComeBack exists
// to prevent. `--answer capabilities=` is what carries the empty answer, and
// it round-trips: channelwizard.splitList reads it as the empty list.
func TestWizardResolve_ManualRouteCarriesADeliberatelyEmptyCapabilityAnswer(t *testing.T) {
	w := stubWizard(t, newWizardWithStub(okAuth(), nil))

	_, err := w.Resolve(context.Background(), defaultInput(), map[string]string{
		keyAgentClass:  "demo-agent",
		keyHasSlackApp: string(routeManual),
		// Present and empty: the question was asked and answered with nothing.
		keyCapabilities: "",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--answer capabilities= \\",
		"an emptied capability answer must be carried into the second run, or it silently re-defaults")
}

// TestWizardResolve_ManualRouteOmitsAnUnaskedCapabilityAnswer is the control on
// it: a question that was never asked has no answer to carry, and inventing
// `--answer capabilities=` there would tell the operator they had chosen
// something they never saw.
func TestWizardResolve_ManualRouteOmitsAnUnaskedCapabilityAnswer(t *testing.T) {
	w := stubWizard(t, newWizardWithStub(okAuth(), nil))

	_, err := w.Resolve(context.Background(), defaultInput(), map[string]string{
		keyAgentClass:  "demo-agent",
		keyHasSlackApp: string(routeManual),
		// keyCapabilities absent entirely.
	})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "--answer capabilities=",
		"never asked is not the same run as answered-with-nothing")
}

// TestWizardInputs_RefusesAnUnattendedProvisioningRunItCannotFinish is
// provisionScreen.RequiresInteraction restored through
// WizardInput.NonInteractive (P5-R19). Without it a --non-interactive run that
// picked the Slack CLI source EXECS `slack auth token` against a nil stdin,
// and the operator is told to supply a configuration token that source ignores.
func TestWizardInputs_RefusesAnUnattendedProvisioningRunItCannotFinish(t *testing.T) {
	cases := []struct {
		name      string
		seeded    map[string]string
		errSubstr string
	}{
		{
			name: "a source that waits on a person: refused, naming the one that does not",
			seeded: map[string]string{
				keyHasSlackApp: string(routeProvision),
				keyTokenSource: "slack-cli",
			},
			errSubstr: "--answer app-token-source=paste",
		},
		{
			name: "no configuration token: refused, saying where to generate one",
			seeded: map[string]string{
				keyHasSlackApp: string(routeProvision),
				keyTokenSource: "paste",
			},
			errSubstr: "Your App Configuration Tokens",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := defaultInput(newAgentClass("demo-agent", "default"))
			in.NonInteractive = true
			in.Seeded = seededAnswers(tc.seeded)

			qs, err := kindWizard(t).Inputs(context.Background(), in)
			require.Error(t, err)
			assert.Nil(t, qs, "a refused run declares no questions")
			assert.Contains(t, err.Error(), tc.errSubstr)
		})
	}
}

// TestWizardInputs_UnattendedProvisioningRunWithATokenIsAllowed is the control
// on the refusal above: a caller who supplied everything the paste source
// needs must not be refused.
func TestWizardInputs_UnattendedProvisioningRunWithATokenIsAllowed(t *testing.T) {
	in := defaultInput(newAgentClass("demo-agent", "default"))
	in.NonInteractive = true
	in.Seeded = seededAnswers(map[string]string{
		keyHasSlackApp: string(routeProvision),
		keyTokenSource: "paste",
		keyConfigToken: "xoxe.xoxp-demo",
	})

	qs, err := kindWizard(t).Inputs(context.Background(), in)
	require.NoError(t, err, "a fully-supplied unattended provisioning run must be allowed to proceed")
	assert.NotEmpty(t, qs)
}

// TestWizardResolve_UnattendedProvisioningRefusesASourceThatWaitsForAPerson is
// the fail-closed half of the same rule, for a client that never called Inputs
// or ignored its refusal. Reaching src.Token would hand a subprocess a terminal
// nobody is at.
//
// THE MESSAGE IS WHAT THIS PINS, not merely that an error came back. Without
// that assertion the test is satisfied by the exact failure it exists to
// prevent: the reverted code execs `slack auth token`, the binary is absent
// from a CI $PATH, and "executable file not found" is an error that created
// nothing — so require.Error and the createCalled assertion both pass while the
// subprocess was in fact launched. On a developer machine that HAS the Slack
// CLI installed, the same reverted code sits in Slack's ticket flow, from a
// unit test, waiting for a challenge code nobody will type.
func TestWizardResolve_UnattendedProvisioningRefusesASourceThatWaitsForAPerson(t *testing.T) {
	fake := &fakeProvisionClient{}
	w := stubWizard(t, newWizardWithProvisioner(fake))

	in := defaultInput()
	in.NonInteractive = true
	// OperatorShell is TRUE so that this test still pins the rule it is named
	// for. A run off the operator's shell is refused one step earlier, for a
	// different reason (see
	// TestWizardResolve_TheSlackCLISourceIsRefusedOffTheOperatorsShell), which
	// would satisfy require.Error while proving nothing about the unattended
	// guard.
	in.OperatorShell = true
	_, err := w.Resolve(context.Background(), in, map[string]string{
		keyAgentClass:  "demo-agent",
		keyHasSlackApp: string(routeProvision),
		keyTokenSource: "slack-cli",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "needs a person at the terminal",
		"the refusal must be the SOURCE's own reason; any other error means src.Token was "+
			"reached and the subprocess ran")
	assert.False(t, fake.createCalled, "nothing may be created for a run that cannot authenticate")
}

// TestWizardResolve_MonitoringKeepPathNeverAsksSlack: there is no plaintext to
// test on the keep path, and what the existing Channel recorded was validated
// when it was written. The stub fails loudly if it is called.
func TestWizardResolve_MonitoringKeepPathNeverAsksSlack(t *testing.T) {
	w := stubWizard(t, newWizardWithStub(nil, assertNotCalledErr))
	in := reSetupInput(existingMonitoringChannel("slack-monitoring-demo-creds"), true)

	derived, err := w.Resolve(context.Background(), in, map[string]string{
		keyHasSlackApp: string(routeHave),
		keyBotToken:    "",
	})
	require.NoError(t, err, "the keep path must not reach auth.test")
	assert.Equal(t, map[string]string{keyBotUserID: "U01OLDBOT"}, derived,
		"the bot identity carries over from the Channel being reconfigured")
}

// TestWizardResolve_MonitoringRefusesTheRoutesItCannotServe keeps
// monitoringSetupScreen's two refusals. Resolve is the first step under this
// contract that can make them: Inputs is one batch, declared before the
// Slack-app question is answered.
func TestWizardResolve_MonitoringRefusesTheRoutesItCannotServe(t *testing.T) {
	cases := []struct {
		name      string
		route     appRoute
		errSubstr string
	}{
		{
			name:      "no app yet: the create-an-app steps, not a token prompt",
			route:     routeManual,
			errSubstr: "no Slack app with a bot token yet",
		},
		{
			name:      "provision: refused, because nothing in this flow can create an app",
			route:     routeProvision,
			errSubstr: "automatic Slack app creation is not offered",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := stubWizard(t, newWizardWithStub(nil, assertNotCalledErr))
			_, err := w.Resolve(context.Background(), monitoringWizardInput(), map[string]string{
				keyHasSlackApp: string(tc.route),
				keyBotToken:    validBotToken,
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errSubstr)
		})
	}
}

// --- Result ---

// TestWizardResult_IsPureOverItsArguments is the strong form of P5-R12's
// purity requirement, aimed at the failure that would be worst here: a Result
// that remembered which FLOW an earlier call described.
//
// primed has had Inputs run for a MONITORING run in another namespace before
// Result is called with an agent-flow WizardInput. A Result that read anything
// that call left behind would build monitoring manifests — a Channel with role
// monitoring, no AgentClass and no app token — from an agent flow's answers.
func TestWizardResult_IsPureOverItsArguments(t *testing.T) {
	in := channelkinds.WizardInput{Namespace: "default"}
	answers := answeredAgentRun(t, "my-channel")

	primed := newWizardWithStub(okAuth(), nil)
	_, err := primed.Inputs(context.Background(), channelkinds.WizardInput{
		K8s: newFakeK8s().Build(), Namespace: "other-ns", Monitoring: true,
	})
	require.NoError(t, err, "Inputs")

	first, err := primed.Result(in, answers)
	require.NoError(t, err)

	second, err := kindWizard(t).Result(in, answers)
	require.NoError(t, err)

	assert.Equal(t, first, second,
		"Result must be a pure function of (in, answers) — not of receiver state an earlier Inputs call left behind")
	require.NotNil(t, first.ChannelManifest)
	assert.Equal(t, "default", first.ChannelManifest.Namespace,
		"the namespace must come from in, not from a field an earlier call set on the receiver")
	assert.Equal(t, "demo-agent", first.ChannelManifest.Spec.AgentClass,
		"an agent-flow in must build an agent-flow Channel, whatever flow Inputs last described")
	assert.Empty(t, first.ChannelManifest.Spec.Role,
		"role=monitoring would be the monitoring branch leaking through the receiver")
}

// TestWizardResult_RefusesWithoutANamespace covers Result's first error path.
func TestWizardResult_RefusesWithoutANamespace(t *testing.T) {
	out, err := kindWizard(t).Result(channelkinds.WizardInput{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "namespace")
	assert.Nil(t, out.ChannelManifest, "a refused Result produces no manifests")
	assert.Nil(t, out.SecretManifest)
}

// TestWizardResult_RefusesAnUnansweredQuestion: a missing answer must not
// produce a Secret with an empty token or a Channel bound to no agent.
func TestWizardResult_RefusesAnUnansweredQuestion(t *testing.T) {
	answers := answeredAgentRun(t, "my-channel")
	delete(answers, keyBotUserID)

	_, err := kindWizard(t).Result(channelkinds.WizardInput{Namespace: "default"}, answers)
	require.Error(t, err)
	assert.Contains(t, err.Error(), keyBotUserID)
}

// TestWizardResult_RefusesACapabilityThisKindCannotEnable keeps
// canonicalCapabilities' membership check. The answer is written verbatim onto
// an AgentClass, so a
// name outside the offered set is a typo or a capability this kind cannot
// deliver, and neither should reach a cluster.
func TestWizardResult_RefusesACapabilityThisKindCannotEnable(t *testing.T) {
	answers := answeredAgentRun(t, "my-channel")
	answers[keyCapabilities] = "not-a-slack-capability"

	_, err := kindWizard(t).Result(channelkinds.WizardInput{Namespace: "default"}, answers)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not-a-slack-capability")
}

// TestWizardResult_BlankTokenOnMonitoringResetupKeepsTheExistingSecret: a
// blank token on re-setup means "keep the one you have", and the CLI is told
// so by a nil SecretManifest. Overwriting a working Secret with an empty token
// is the failure this guards.
func TestWizardResult_BlankTokenOnMonitoringResetupKeepsTheExistingSecret(t *testing.T) {
	in := reSetupInput(existingMonitoringChannel("slack-monitoring-demo-creds"), true)

	out, err := kindWizard(t).Result(in, map[string]string{
		keyHasSlackApp:          string(routeHave),
		keyBotToken:             "",
		keyDestinationChannelID: "C0OLDCHAN",
		keyBotUserID:            "U01OLDBOT",
	})
	require.NoError(t, err)
	assert.Nil(t, out.SecretManifest,
		"a blank token on re-setup means keep the existing creds, not write empty ones")
	require.NotNil(t, out.ChannelManifest)
	assert.True(t, out.ReplaceExisting, "the CLI's refuse-to-overwrite guard has to be relaxed for this")
	assert.Equal(t, "slack-monitoring-demo", out.ChannelManifest.Name,
		"the Channel being reconfigured keeps its own name")
}

// --- What the run summary says ---

// TestWizardSummary_EveryFlowStatesItsDecisionsInOrder pins the summary's
// SHAPE for all three flows: which lines it carries and in what order.
//
// This kind runs no code of its own while the questions are being answered, so
// every line here has to be stated by Result or it is never written at all
// (channelkinds.WizardOutput.Summary) — and the summary is read top to bottom,
// so a line silently dropped or reordered changes what the operator reads.
// What each line CARRIES is asserted by the tests below, which is why this one
// stops at the labels.
func TestWizardSummary_EveryFlowStatesItsDecisionsInOrder(t *testing.T) {
	cases := []struct {
		name    string
		in      channelkinds.WizardInput
		answers map[string]string
		want    []string
	}{
		{
			name:    "agent flow",
			in:      channelkinds.WizardInput{Namespace: "default"},
			answers: answeredAgentRun(t, "my-channel"),
			want: []string{
				"AgentClass", "Capabilities", "Bot token", "App token",
				"Slack team", "Bot", "Channel",
			},
		},
		{
			name: "monitoring, first setup",
			in:   monitoringWizardInput(),
			answers: map[string]string{
				keyHasSlackApp:            string(routeHave),
				keyBotToken:               validBotToken,
				keyDestinationChannelID:   "C0123ABCDEF",
				wizardkeys.KeyChannelName: "my-mon",
				keyBotUserID:              "U0DEMO123",
				keyTeamName:               "Demo Workspace",
				keyBotUserName:            "demo-bot",
			},
			want: []string{"Bot token", "Slack team", "Bot", "Slack channel", "Channel"},
		},
		{
			// The keep branch has two lines that exist on no other path: the
			// token was not replaced, and the identity was carried over rather
			// than fetched — so there is no workspace name to report.
			name: "monitoring re-setup, keeping the stored token",
			in:   reSetupInput(existingMonitoringChannel("slack-monitoring-demo-creds"), true),
			answers: map[string]string{
				keyHasSlackApp:          string(routeHave),
				keyBotToken:             "",
				keyDestinationChannelID: "C0OLDCHAN",
				keyBotUserID:            "U01OLDBOT",
			},
			want: []string{"Bot token", "Bot", "Slack channel", "Channel"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := kindWizard(t).Result(tc.in, tc.answers)
			require.NoError(t, err, "Result")

			labels := make([]string, 0, len(out.Summary))
			for _, n := range out.Summary {
				labels = append(labels, n.Label)
				assert.NotEmptyf(t, n.Value, "the %q line must say something", n.Label)
			}
			assert.Equal(t, tc.want, labels)
		})
	}
}

// TestWizardSummary_MonitoringKeepPathSaysWhatItKept: the two lines unique to
// re-setup have to be READABLE, not merely present — an operator who sees
// "Bot token" with a masked value would conclude their token was replaced.
func TestWizardSummary_MonitoringKeepPathSaysWhatItKept(t *testing.T) {
	out, err := kindWizard(t).Result(
		reSetupInput(existingMonitoringChannel("slack-monitoring-demo-creds"), true),
		map[string]string{
			keyHasSlackApp:          string(routeHave),
			keyBotToken:             "",
			keyDestinationChannelID: "C0OLDCHAN",
			keyBotUserID:            "U01OLDBOT",
		})
	require.NoError(t, err, "Result")

	tok, ok := summaryValue(out.Summary, "Bot token")
	require.True(t, ok)
	assert.Equal(t, "kept — the stored credentials are unchanged", tok)

	bot, ok := summaryValue(out.Summary, "Bot")
	require.True(t, ok)
	assert.Equal(t, "U01OLDBOT — kept from the existing Channel", bot,
		"nothing asked Slack on this path, so there is no @name to render — and \"@ ()\" is worse than the half that exists")

	name, ok := summaryValue(out.Summary, "Channel")
	require.True(t, ok)
	assert.Equal(t, "slack-monitoring-demo — updated in place", name,
		"re-setup rewrites a Channel rather than creating one, and the summary has to say so")
}

// --- P5-R15: the three masked credentials ---

// TestWizardSummary_AgentFlowNeverPutsARawTokenInScrollback is the security
// gate on the summary. SummaryNote.Value reaches plain scrollback verbatim
// — the client filters nothing, and the summary is the block
// deliberately left behind after the alt-screen is released, so it outlives
// the run in the operator's terminal history.
//
// Nothing between a SummaryNote and the operator inspects the value, so a
// dropped credmask.Mask wrapper here leaks a live token with nothing else in
// the system to catch it.
//
// Both halves are asserted per token. The raw value must be absent from the
// RENDERED block, which is the actual leak; and the recorded value must equal
// credmask.Mask of it, which is what fails if the line were merely omitted or
// replaced by a constant.
func TestWizardSummary_AgentFlowNeverPutsARawTokenInScrollback(t *testing.T) {
	out, err := kindWizard(t).Result(
		channelkinds.WizardInput{Namespace: "default"}, answeredAgentRun(t, "my-channel"))
	require.NoError(t, err)

	rendered := renderedSummary(t, out.Summary)

	cases := []struct {
		label string
		raw   string
	}{
		{label: "Bot token", raw: validBotToken},
		{label: "App token", raw: validAppToken},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			got, ok := summaryValue(out.Summary, tc.label)
			require.Truef(t, ok, "the summary must still record a %q line", tc.label)
			assert.Equal(t, credmask.Mask(tc.raw), got,
				"the recorded value must be the masked form, not the raw credential")
			assert.NotContains(t, rendered, tc.raw,
				"the raw %s reached plain scrollback, where it outlives the run", tc.label)
			assert.Contains(t, rendered, credmask.Mask(tc.raw),
				"and the masked form must be what is shown, so the line is not merely missing")
		})
	}
}

// TestWizardSummary_MonitoringNeverPutsARawTokenInScrollback is the same gate
// for the monitoring flow's single token, the third of the three values this
// kind must mask before stating.
func TestWizardSummary_MonitoringNeverPutsARawTokenInScrollback(t *testing.T) {
	out, err := kindWizard(t).Result(monitoringWizardInput(), map[string]string{
		keyHasSlackApp:            string(routeHave),
		keyBotToken:               validBotToken,
		keyDestinationChannelID:   "C0123ABCDEF",
		wizardkeys.KeyChannelName: "my-mon",
		keyBotUserID:              "U0DEMO123",
		keyTeamName:               "Demo Workspace",
		keyBotUserName:            "demo-bot",
	})
	require.NoError(t, err)

	rendered := renderedSummary(t, out.Summary)

	got, ok := summaryValue(out.Summary, "Bot token")
	require.True(t, ok, "the summary must still record a \"Bot token\" line")
	assert.Equal(t, credmask.Mask(validBotToken), got,
		"the recorded value must be the masked form, not the raw credential")
	assert.NotContains(t, rendered, validBotToken,
		"the raw bot token reached plain scrollback, where it outlives the run")
	assert.Contains(t, rendered, credmask.Mask(validBotToken),
		"and the masked form must be what is shown, so the line is not merely missing")
}

// TestWizardSummary_ProvisionedRunRecordsTheAppItCreated: provisioning is the
// one step of this flow that leaves something behind in the world, and no
// Slack API lists a user's apps — so this line and the Channel's
// spec.slack.appId are the only record that the app exists. Losing it is
// unrecoverable in a way no other missing summary line is.
func TestWizardSummary_ProvisionedRunRecordsTheAppItCreated(t *testing.T) {
	answers := answeredAgentRun(t, "my-channel")
	answers[keyHasSlackApp] = string(routeProvision)
	answers[keyAppID] = "A0DEMO"

	out, err := kindWizard(t).Result(channelkinds.WizardInput{Namespace: "default"}, answers)
	require.NoError(t, err)

	got, ok := summaryValue(out.Summary, "Slack app")
	require.True(t, ok, "a run that created an app must say so")
	assert.Equal(t, "created and installed (A0DEMO)", got)

	require.NotNil(t, out.ChannelManifest.Spec.Slack)
	assert.Equal(t, "A0DEMO", out.ChannelManifest.Spec.Slack.AppID,
		"and the other half of that record is on the Channel itself")
}

// TestWizardSummary_PastedRunRecordsNoApp is the other half: the "Slack app"
// line exists only for a run that created one, exactly as postSetupNotes reads
// the same signal. A run whose operator pasted their own tokens created
// nothing.
func TestWizardSummary_PastedRunRecordsNoApp(t *testing.T) {
	out, err := kindWizard(t).Result(
		channelkinds.WizardInput{Namespace: "default"}, answeredAgentRun(t, "my-channel"))
	require.NoError(t, err)

	_, ok := summaryValue(out.Summary, "Slack app")
	assert.False(t, ok, "no app was created, so there is nothing to record")

	require.NotNil(t, out.ChannelManifest.Spec.Slack)
	assert.Empty(t, out.ChannelManifest.Spec.Slack.AppID)
}

// TestWizardResult_ChannelIsBoundAndCredentialed is the plain happy-path
// assertion on the data path, so a failure in the cross-contract gate above
// can be told apart from the manifests simply being wrong.
func TestWizardResult_ChannelIsBoundAndCredentialed(t *testing.T) {
	out, err := kindWizard(t).Result(
		channelkinds.WizardInput{Namespace: "default"}, answeredAgentRun(t, "my-channel"))
	require.NoError(t, err)

	require.NotNil(t, out.SecretManifest)
	assert.Equal(t, "my-channel-creds", out.SecretManifest.Name)
	assert.Equal(t, validBotToken, string(out.SecretManifest.Data[SecretKeyBotToken]))
	assert.Equal(t, validAppToken, string(out.SecretManifest.Data[SecretKeyAppToken]))

	require.NotNil(t, out.ChannelManifest)
	ch := out.ChannelManifest
	assert.Equal(t, "my-channel", ch.Name)
	assert.Equal(t, "slack", ch.Spec.Kind)
	assert.Equal(t, "demo-agent", ch.Spec.AgentClass)
	assert.Empty(t, ch.Spec.Role, "the agent flow builds a Channel with no explicit role")
	require.NotNil(t, ch.Spec.Slack)
	assert.Equal(t, "socket", ch.Spec.Slack.Mode)
	assert.Equal(t, "U0DEMO123", ch.Spec.Slack.BotUserID)
}
