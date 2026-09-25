package channelwizard

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardrun"
)

// TestChannelCreate_ProvisioningAnswerKeysFollowTheSeededRoute is the
// dispatcher-level test for the ruling that gave WizardInput a Seeded field
// (Task 5): --answer slackapp=provision must make checkAnswerKeys accept
// app-token-source and app-config-token, and --answer slackapp=true (the
// user already has an app) must make it reject them — the two directions of
// the route read from WizardInput.Seeded that decide which pair of credential
// questions the slack kind declares.
//
// Both cases stop at checkAnswerKeys — the exact function whose verdict is
// being proved — rather than driving the whole run. That is deliberate: the
// real slack Kind's Wizard() wires a live appprovision.HTTPClient with no test
// seam, so a fully-seeded provisioning run would reach the network. Stopping
// at the check itself is what keeps this test hermetic AND still a genuine
// proof of the dispatcher-level contract; the slack package proves completion
// against a fake client (TestWizard_Provision_FillsBothTokensAndNeverAsks).
func TestChannelCreate_ProvisioningAnswerKeysFollowTheSeededRoute(t *testing.T) {
	t.Run("slackapp=provision: the provisioning answer keys are accepted", func(t *testing.T) {
		seeded, declared := declaredKeysForSeededRun(t, "slack",
			"agentclass=demo-agent",
			"slackapp=provision",
			"capabilities=attachments",
			"app-token-source=paste",
			"app-config-token=xoxe.xoxp-demo-config-token",
		)
		assert.Contains(t, declared, "app-token-source", "the provisioning route must declare this key")
		assert.Contains(t, declared, "app-config-token", "and this one")
		assert.NoError(t, checkAnswerKeys("slack", seeded.Keys, declared),
			"checkAnswerKeys must accept every key seeded here when the seeded route is provisioning")
	})

	t.Run("slackapp=true: the provisioning answer keys are refused", func(t *testing.T) {
		seeded, declared := declaredKeysForSeededRun(t, "slack",
			"agentclass=demo-agent",
			"slackapp=true",
			"app-token-source=paste",
		)
		assert.NotContains(t, declared, "app-token-source",
			"the have-an-app route must not declare a provisioning-only key")

		err := checkAnswerKeys("slack", seeded.Keys, declared)
		require.Error(t, err, "an answer key the kind does not declare this run must be refused")
		assert.Contains(t, err.Error(), "is not a question the slack wizard asks",
			"checkAnswerKeys must reject app-token-source when the seeded route is NOT provisioning")
		assert.Contains(t, err.Error(), "app-token-source", "the refusal must name the offending key")
	})

	// The third reading, and the one the interactive route needs: when NO flag
	// has settled the route, EVERY route's keys are accepted, because the
	// operator has not yet chosen and any of them could still be the run's.
	//
	// That is the up-front half of a branching question set. The kind declares
	// both pairs so that no key a caller may legitimately supply is refused
	// before the choice is made, and gates which pair is ASKED on the answer
	// (oap.Question.AskWhen). Narrowing the declared set to what the flags
	// happened to imply is what made an interactive operator picking "create
	// the app for me" unable to reach the provisioning questions at all.
	t.Run("no seeded route: both routes' answer keys are accepted", func(t *testing.T) {
		seeded, declared := declaredKeysForSeededRun(t, "slack",
			"agentclass=demo-agent",
			"app-token-source=paste",
			"bot-token=xoxb-1234567890123-1234567890123-abcdefghijklmnopqrstuvwx",
		)
		assert.Contains(t, declared, "app-token-source", "the provisioning route is still on the table")
		assert.Contains(t, declared, "bot-token", "and so is the have-an-app route")
		assert.NoError(t, checkAnswerKeys("slack", seeded.Keys, declared),
			"a caller cannot be refused a key for a route they have not been asked to rule out yet")
	})
}

// declaredKeysForSeededRun is the dispatcher's own derivation of "which
// --answer keys may this run supply?", run against a registered kind seeded
// exactly as the flags would seed it.
//
// It goes through Seed, Inputs, Handoff and wizardrun.AllInputs — the same
// four steps Run takes — rather than calling Inputs alone: a
// handoff's fallback questions are as seedable as any other, and a derivation
// that forgot them would refuse the flags that make an unattended github run
// possible at all.
func declaredKeysForSeededRun(t *testing.T, kindName string, answers ...string) (Seeded, []string) {
	t.Helper()

	kind, ok := registry.Get(kindName)
	require.Truef(t, ok, "the %s kind must be registered in the oap binary", kindName)

	_, seeded, err := Seed("", answers)
	require.NoError(t, err, "Seed")

	in := channelkinds.WizardInput{Namespace: "default", Seeded: seeded.Values}
	inputs, err := kind.Wizard().Inputs(context.Background(), in)
	require.NoError(t, err, "Inputs")
	handoff, err := kind.Wizard().Handoff(context.Background(), in)
	require.NoError(t, err, "Handoff")

	return seeded, inputAnswerKeys(wizardrun.AllInputs(inputs, handoff))
}

// TestChannelCreate_ProvisioningRouteWithPreSeededTokensIsAccepted: a caller
// who provisioned once, saved bot-token and app-token, and re-runs the same
// script with slackapp=provision still set must not be refused by
// checkAnswerKeys. The kind itself honours that combination — both tokens in
// hand means there is nothing to provision — so refusing it here would tell a
// caller that a flag they are correctly using "is not a question the slack
// wizard asks".
//
// Both subtests seed the SAME two tokens, on the two routes that have always
// accepted them, so the second is the regression guard: making the credential
// questions route-conditional must not have narrowed what the have-an-app
// route accepts.
func TestChannelCreate_ProvisioningRouteWithPreSeededTokensIsAccepted(t *testing.T) {
	const (
		seededBotToken = "xoxb-1234567890123-1234567890123-abcdefghijklmnopqrstuvwx"
		seededAppToken = "xapp-1-AAAAAAAAA-1234567890123-abcdefghijklmnopqrstuvwxyz"
	)

	t.Run("slackapp=provision with both tokens seeded: bot-token/app-token are accepted", func(t *testing.T) {
		seeded, declared := declaredKeysForSeededRun(t, "slack",
			"agentclass=demo-agent",
			"slackapp=provision",
			"capabilities=attachments",
			"bot-token="+seededBotToken,
			"app-token="+seededAppToken,
		)
		assert.Contains(t, declared, "bot-token",
			"a caller who already has both tokens must be able to seed them, even while re-running with the provisioning route")
		assert.Contains(t, declared, "app-token", "so must this one")
		assert.NoError(t, checkAnswerKeys("slack", seeded.Keys, declared),
			"checkAnswerKeys must accept bot-token/app-token seeded alongside slackapp=provision")
	})

	t.Run("slackapp=true with both tokens seeded: unchanged, still accepted", func(t *testing.T) {
		seeded, declared := declaredKeysForSeededRun(t, "slack",
			"agentclass=demo-agent",
			"slackapp=true",
			"bot-token="+seededBotToken,
			"app-token="+seededAppToken,
		)
		assert.Contains(t, declared, "bot-token", "the have-an-app route has always declared these")
		assert.Contains(t, declared, "app-token", "so must this one")
		assert.NoError(t, checkAnswerKeys("slack", seeded.Keys, declared),
			"the route-conditional question set must not have changed behavior on the have-an-app route")
	})
}

// TestCheckAnswerKeys covers both directions of the check, including the one
// way it must NOT fire — an always-allowed `--name` — and the case a kind that
// asks nothing produces.
func TestCheckAnswerKeys(t *testing.T) {
	cases := []struct {
		name     string
		seeded   []string
		declared []string
		wantErr  string
	}{
		{
			name:     "key the flow declares: accepted",
			seeded:   []string{"agentclass"},
			declared: []string{"agentclass", "bot-token"},
		},
		{
			name:     "key the flow does not declare: refused, listing what it does ask",
			seeded:   []string{"agentclas"},
			declared: []string{"agentclass", "bot-token"},
			wantErr:  "it asks: agentclass, bot-token",
		},
		{
			// Inputs is the whole question set in one call, so an empty
			// declared set means "this kind asks nothing" — not "it did not
			// say". The fake test kind is exactly that; waving its keys
			// through would put the one kind that can never use an answer
			// outside the check that exists to catch a key nothing reads.
			name:     "kind that asks nothing: every --answer is refused, and the message says why",
			seeded:   []string{"authzsubject"},
			declared: nil,
			wantErr:  "it asks nothing",
		},
		{
			name:     "kind that asks nothing: --name is still allowed",
			seeded:   []string{wizardkeys.KeyChannelName},
			declared: nil,
		},
		{
			name:     "the Channel name is always allowed, even when unasked this run",
			seeded:   []string{wizardkeys.KeyChannelName},
			declared: []string{"destination"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkAnswerKeys("demo-kind", tc.seeded, tc.declared)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Contains(t, err.Error(), "demo-kind", "the refusal must name the kind that was asked")
		})
	}
}

// TestAnswerFlagHint: the Channel name has its own flag, so a hint that told
// the user to reach it through the generic form would be worse advice than the
// one it replaced.
func TestAnswerFlagHint(t *testing.T) {
	cases := []struct {
		name string
		keys []string
		want string
	}{
		{name: "one ordinary key: the generic flag", keys: []string{"agentclass"}, want: "--answer agentclass=<value>"},
		{
			name: "a screen asking two: both flags, in screen order",
			keys: []string{"bot-token", "app-token"},
			want: "--answer bot-token=<value> --answer app-token=<value>",
		},
		{name: "the Channel name: its own flag", keys: []string{wizardkeys.KeyChannelName}, want: "--name <value>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, answerFlagHint(tc.keys))
		})
	}
}
