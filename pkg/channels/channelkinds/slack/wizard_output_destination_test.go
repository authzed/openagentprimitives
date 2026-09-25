package slack

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/kindtest"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/outputbind"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// demoDestination is the Slack channel ID these tests post into. Slack's own
// shape — a C-prefixed uppercase ID — because the wizard now checks it.
const demoDestination = "C0DEMO123"

// outputRunInput is defaultInput for a run whose Channel will carry
// role=output: the one role whose Channel is somebody else's destination and
// so can never be told where to post by an inbound message.
func outputRunInput(t *testing.T) channelkinds.WizardInput {
	t.Helper()
	in := defaultInput(newAgentClass("demo-agent", "default"))
	in.Role = spiceboxv1alpha1.ChannelRoleOutput
	in.Seeded = seededAnswers(map[string]string{keyHasSlackApp: string(routeHave)})
	return in
}

// typedOutputAnswers is typedAgentAnswers plus the destination an output run
// is asked for.
func typedOutputAnswers(t *testing.T, channelName, destination string) map[string]string {
	t.Helper()
	answers := typedAgentAnswers(t, channelName)
	answers[keyDestinationChannelID] = destination
	return answers
}

// TestWizardInputs_AnOutputChannelIsAskedWhereToPost is the gap this file
// closes. A role=output Channel is the destination for someone ELSE's input,
// so nothing inbound to it ever names a Slack channel — the operator has to,
// and until now was never asked.
//
// Pinned as a whole shape rather than as "a question named destination exists"
// so that the question's position, type and requiredness are pinned too: it is
// asked after the credentials and before the Channel name, which is the order
// an operator answers them in.
func TestWizardInputs_AnOutputChannelIsAskedWhereToPost(t *testing.T) {
	qs, err := kindWizard(t).Inputs(context.Background(), outputRunInput(t))
	require.NoError(t, err)

	kindtest.AssertPromptShapes(t, qs, []kindtest.PromptShape{
		{Name: "agentclass", Type: oap.QEnum, Prompt: agentClassPrompt, Required: true},
		{Name: "slackapp", Type: oap.QEnum, Prompt: agentSlackAppPrompt, Required: true},
		{Name: "capabilities", Type: oap.QResourceList, Prompt: capabilityPrompt, Required: true},
		{Name: "bot-token", Type: oap.QSecret, Prompt: botTokenPrompt, Required: true},
		{Name: "app-token", Type: oap.QSecret, Prompt: appTokenPrompt, Required: true},
		{Name: "destination", Type: oap.QString, Prompt: agentDestinationPrompt, Required: true},
		{Name: "name", Type: oap.QString, Prompt: wizardkeys.ChannelNamePrompt, Required: true},
	})
}

// TestWizardInputs_ARoleThatIsToldWhereToPostIsNotAsked is the noise-regression
// direction, and it is the reason the question is gated on the role at all
// rather than always asked.
//
// Every one of these runs produces a Channel that is its own origin (both, and
// the CRD default an unspecified role takes) or that is nobody's destination
// (input). An inbound message carries the thread to reply into, so a
// destination question would be a Slack channel ID demanded of every operator
// who ever ran `oap channel create --kind slack`.
func TestWizardInputs_ARoleThatIsToldWhereToPostIsNotAsked(t *testing.T) {
	cases := []struct {
		name string
		role string
	}{
		{name: "no declared role: the Channel takes the CRD's both default, which is its own destination", role: ""},
		{name: "role=both: origin and destination in one, so the inbound names the thread", role: spiceboxv1alpha1.ChannelRoleBoth},
		{name: "role=input: produces no outbound of its own; its reply lands in a different Channel", role: spiceboxv1alpha1.ChannelRoleInput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := outputRunInput(t)
			in.Role = tc.role

			qs, err := kindWizard(t).Inputs(context.Background(), in)
			require.NoError(t, err)

			for _, q := range qs {
				assert.NotEqual(t, keyDestinationChannelID, q.Name,
					"a Channel whose destination arrives with the inbound must not be asked for one")
			}
		})
	}
}

// TestWizardInputs_ASeededDestinationIsStillDeclared is the scripted run.
//
// The key has to be DECLARED even when the flag already answered it:
// channelwizard.checkAnswerKeys refuses an --answer naming a key this run's
// question set does not contain, and renderQuestions drops the screen rather
// than the declaration. A question dropped at declaration time would refuse
// `--answer destination=…` on precisely the run that supplied it.
func TestWizardInputs_ASeededDestinationIsStillDeclared(t *testing.T) {
	in := outputRunInput(t)
	in.Seeded = seededAnswers(map[string]string{
		keyHasSlackApp:          string(routeHave),
		keyDestinationChannelID: demoDestination,
	})

	qs, err := kindWizard(t).Inputs(context.Background(), in)
	require.NoError(t, err)

	var found bool
	for _, q := range qs {
		if q.Name == keyDestinationChannelID {
			found = true
		}
	}
	assert.True(t, found, "--answer destination=… is only accepted for a key the run declares")
}

// TestWizardResult_TheAnsweredDestinationReachesTheChannel: the answer is the
// whole point, so it has to land on the field the outbound relay reads.
func TestWizardResult_TheAnsweredDestinationReachesTheChannel(t *testing.T) {
	in := outputRunInput(t)
	out, err := resolvedAgentRun(t, newWizardWithStub(okAuth(), nil), in,
		typedOutputAnswers(t, "demo-out", demoDestination))
	require.NoError(t, err, "wizard run")

	require.NotNil(t, out.ChannelManifest)
	require.NotNil(t, out.ChannelManifest.Spec.Slack, "the slack block")
	require.NotNil(t, out.ChannelManifest.Spec.Slack.OutputDefaults, "spec.slack.outputDefaults")
	assert.Equal(t, demoDestination, out.ChannelManifest.Spec.Slack.OutputDefaults.ChannelID)
	assert.Empty(t, out.ChannelManifest.Spec.Slack.OutputDefaults.ThreadStrategy,
		"the strategy is left to the CRD's own new-thread-per-session default, so a re-run applies byte-identically")
}

// TestWizardResult_TheOutputChannelSatisfiesTheAnchorTheControllerDemands is
// the point of the change, and the one assertion that spans the two sides.
//
// outputbind.Anchor IS the Channel controller's rule: it is what
// ReasonChannelOutputDestinationMissing is reported from, and what a
// role=input Channel's binding check calls on its resolved target. Asserting
// the wizard's own output against it means the wizard and the controller
// cannot drift into disagreeing about what a usable destination is.
func TestWizardResult_TheOutputChannelSatisfiesTheAnchorTheControllerDemands(t *testing.T) {
	in := outputRunInput(t)
	out, err := resolvedAgentRun(t, newWizardWithStub(okAuth(), nil), in,
		typedOutputAnswers(t, "demo-out", demoDestination))
	require.NoError(t, err, "wizard run")
	require.NotNil(t, out.ChannelManifest)

	key, external, err := outputbind.Anchor(out.ChannelManifest)
	require.NoError(t, err, "the Channel the wizard produced must satisfy the controller's own destination rule")
	assert.Equal(t, "channel:"+demoDestination, key)
	assert.Equal(t, demoDestination, external["channel_id"])
}

// TestWizardResult_AnOutputChannelWithNoDestinationIsRefusedByName is the
// fail-closed half: a client that skipped the questions, or a truncated input
// script, must not produce the very Channel the controller then refuses.
//
// It names the answer key, because the caller who reaches here without one is
// seeding answers from flags and needs to be told which flag was missing.
func TestWizardResult_AnOutputChannelWithNoDestinationIsRefusedByName(t *testing.T) {
	in := outputRunInput(t)
	_, err := resolvedAgentRun(t, newWizardWithStub(okAuth(), nil), in,
		typedAgentAnswers(t, "demo-out"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), keyDestinationChannelID)
	assert.Contains(t, err.Error(), "C0", "the refusal shows what the answer looks like")
}

// TestWizardResult_ARoleThatWasNotAskedIsNotRequiredToAnswer keeps the refusal
// above from becoming a refusal of every ordinary Slack channel: a run that
// was never asked for a destination must not be refused for not having one.
func TestWizardResult_ARoleThatWasNotAskedIsNotRequiredToAnswer(t *testing.T) {
	in := defaultInput(newAgentClass("demo-agent", "default"))
	out, err := resolvedAgentRun(t, newWizardWithStub(okAuth(), nil), in,
		typedAgentAnswers(t, "demo-both"))
	require.NoError(t, err, "an unroled run is the CRD's both default and supplies its own destination")
	require.NotNil(t, out.ChannelManifest)
	require.NotNil(t, out.ChannelManifest.Spec.Slack)
	assert.Nil(t, out.ChannelManifest.Spec.Slack.OutputDefaults,
		"a Channel nobody asked a destination of must not be given one")
}

// TestWizardResult_RefusesAnythingThatIsNotASlackChannelID is the shape check.
//
// A channel wizard's questions carry no validation any client evaluates
// (channelkinds.ValidateInputs refuses Question.Validation outright), so this
// is the only thing between a channel NAME — which is what an operator reaches
// for, and which no Slack API call accepts here — and a delivery failure that
// waits until the agent has finished its first piece of work.
func TestWizardResult_RefusesAnythingThatIsNotASlackChannelID(t *testing.T) {
	cases := []struct {
		name        string
		destination string
		wantErr     bool
	}{
		{name: "a public channel ID: accepted", destination: "C0DEMO123"},
		{name: "a private channel ID: accepted, a bot posts to one the same way", destination: "G0DEMO1234"},
		{name: "a DM channel ID: accepted", destination: "D0DEMO1234"},
		{name: "a channel name with a hash: refused, it is not an ID and nothing resolves it", destination: "#demo-room", wantErr: true},
		{name: "a bare channel name: refused for the same reason", destination: "demo-room", wantErr: true},
		{name: "the whole Copy-link URL: refused rather than posting to a channel that does not exist", destination: "https://demo-org.slack.test/archives/C0DEMO123", wantErr: true},
		{name: "a lowercase ID: refused, Slack's own IDs are uppercase", destination: "c0demo123", wantErr: true},
		{name: "a user ID pasted into the channel field: refused", destination: "U0DEMO123", wantErr: true},
		{name: "too short to be an ID: refused", destination: "C0DEM", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := outputRunInput(t)
			out, err := resolvedAgentRun(t, newWizardWithStub(okAuth(), nil), in,
				typedOutputAnswers(t, "demo-out", tc.destination))
			if tc.wantErr {
				require.Error(t, err)
				assert.NotContains(t, err.Error(), "\n\n",
					"a shape refusal is one sentence, not a wall of setup steps")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, out.ChannelManifest.Spec.Slack.OutputDefaults)
			assert.Equal(t, tc.destination, out.ChannelManifest.Spec.Slack.OutputDefaults.ChannelID)
		})
	}
}

// TestManualRoute_TheComeBackCommandRebuildsTheSameChannel is the drift
// dataComeBack exists to prevent, applied to the two things a role=output run
// now depends on.
//
// The manual route ENDS the run: nothing is applied, and the operator comes
// back with a whole new invocation built from the command this refusal prints.
// A command that dropped --role would produce a Channel on ChannelSpec.Role's
// `both` default — not an output-binding candidate — and one that dropped the
// destination would produce the very Channel the controller refuses. Between
// them that is the original defect, reconstructed by following the
// instructions.
func TestManualRoute_TheComeBackCommandRebuildsTheSameChannel(t *testing.T) {
	in := outputRunInput(t)
	answers := typedOutputAnswers(t, "demo-out", demoDestination)
	answers[keyHasSlackApp] = string(routeManual)

	_, err := kindWizard(t).Resolve(context.Background(), in, answers)
	require.Error(t, err, "the manual route ends the run by handing over the manifest")

	assert.Contains(t, err.Error(), "--role "+spiceboxv1alpha1.ChannelRoleOutput,
		"the second run must build a Channel with the same role, or the output binding resolves to nothing")
	assert.Contains(t, err.Error(), "--answer "+keyDestinationChannelID+"="+demoDestination,
		"and with the destination this run already collected, rather than asking for it again")
}

// TestManualRoute_TheComeBackCommandNamesNoRoleWhenNoneWasDeclared keeps the
// line above from appearing on every run: `oap channel create --kind slack`
// with no --role must get back the command it typed, not one that pins a role
// it never asked for.
func TestManualRoute_TheComeBackCommandNamesNoRoleWhenNoneWasDeclared(t *testing.T) {
	in := defaultInput(newAgentClass("demo-agent", "default"))
	answers := typedAgentAnswers(t, "demo-both")
	answers[keyHasSlackApp] = string(routeManual)

	_, err := kindWizard(t).Resolve(context.Background(), in, answers)
	require.Error(t, err)

	assert.NotContains(t, err.Error(), "--role")
	assert.NotContains(t, err.Error(), "--answer "+keyDestinationChannelID)
}

// TestWizardResolve_RefusesABadDestinationBeforeCreatingASlackApp is P5-R21
// applied to this answer.
//
// Resolve is where the provisioning route creates and installs a real Slack
// app, and Slack has no API that lists a user's apps afterwards. A destination
// that Result was always going to refuse must therefore be refused in FRONT of
// that step, or a typo in a channel ID costs the operator an app nothing can
// find again.
func TestWizardResolve_RefusesABadDestinationBeforeCreatingASlackApp(t *testing.T) {
	in := outputRunInput(t)
	in.Seeded = seededAnswers(map[string]string{keyHasSlackApp: string(routeProvision)})

	answers := typedOutputAnswers(t, "demo-out", "#demo-room")
	answers[keyHasSlackApp] = string(routeProvision)
	// No tokens: a provisioning run mints them, so reaching the provisioning
	// step at all is what this test must NOT do.
	delete(answers, keyBotToken)
	delete(answers, keyAppToken)

	w := &slackWizard{
		authFactory: func(string) authTester { return &stubAuthTester{resp: okAuth()} },
		// A nil provisioning client: reaching the provisioning step would
		// fail on it, so a refusal that names the destination proves the run
		// stopped before it.
	}
	_, err := w.Resolve(context.Background(), in, answers)
	require.Error(t, err)
	assert.Contains(t, err.Error(), keyDestinationChannelID,
		"the destination is refused by name, before the step that creates an app nothing can list")
	assert.Contains(t, err.Error(), "#demo-room")
}

// TestWizardSummary_NamesTheDestinationItWillPostTo: the summary is what the
// run DECIDED, and where the agent's work will appear is one of the decisions
// an operator most needs to read back.
func TestWizardSummary_NamesTheDestinationItWillPostTo(t *testing.T) {
	in := outputRunInput(t)
	out, err := resolvedAgentRun(t, newWizardWithStub(okAuth(), nil), in,
		typedOutputAnswers(t, "demo-out", demoDestination))
	require.NoError(t, err)

	assert.Contains(t, renderedSummary(t, out.Summary), demoDestination,
		"the summary names the Slack channel this Channel posts into")
}

// TestOutputDestinationNeeded is the predicate itself, over every role a
// Channel can hold — including the ones no flag can produce today, so a role
// added to the enum later cannot silently inherit an answer here.
func TestOutputDestinationNeeded(t *testing.T) {
	cases := []struct {
		role string
		want bool
	}{
		{role: "", want: false},
		{role: spiceboxv1alpha1.ChannelRoleBoth, want: false},
		{role: spiceboxv1alpha1.ChannelRoleInput, want: false},
		{role: spiceboxv1alpha1.ChannelRoleOutput, want: true},
		{role: spiceboxv1alpha1.ChannelRoleMonitoring, want: false},
	}
	for _, tc := range cases {
		t.Run("role="+tc.role, func(t *testing.T) {
			assert.Equal(t, tc.want, outputDestinationNeeded(tc.role))
		})
	}
}

// TestWizardInputs_TheDestinationGuidanceSaysWhereToFindTheID: the operator has
// to be told which of the two things Slack shows them is wanted, because a
// channel name is right there beside the ID and is not the same value.
func TestWizardInputs_TheDestinationGuidanceSaysWhereToFindTheID(t *testing.T) {
	qs, err := kindWizard(t).Inputs(context.Background(), outputRunInput(t))
	require.NoError(t, err)

	var desc string
	for _, q := range qs {
		if q.Name == keyDestinationChannelID {
			desc = q.Description
		}
	}
	require.NotEmpty(t, desc, "the destination question carries guidance")
	assert.Contains(t, desc, "Copy link", "where Slack keeps the ID")
	assert.True(t, strings.Contains(desc, "C0"), "what the answer looks like")
}
