package steelthread_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// withMintedCredential swaps the fixture identity's stored credential for one
// whose type MINTS — the shape a GitHub App agent has, and the one a fixture
// cannot stand in for.
func withMintedCredential(f *steelthread.FixtureInput) {
	f.Identity.Spec.Credentials = []spiceboxv1alpha1.AgentCredential{{
		Name: "demoforge-app",
		Type: "githubApp",
		GitHubApp: &spiceboxv1alpha1.GitHubAppCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretRef{Name: "demoforge-app-creds"},
		},
	}}
	// The class's MCP server draws on the credential by name, so the fixture
	// stays internally consistent.
	f.MCPServers[0].Spec.Auth.Credential = "demoforge-app"
}

// TestRewriteFixture_ReportsAMintedCredential is the rewrite half of the
// mintable check: the function that EMITS a credential is the function that says
// a placeholder could not stand in for it.
//
// The negative control is the load-bearing half. A check that named every
// credential would refuse every capture, and the two cases differ only in the
// credential's TYPE — which is the fact credkind.Kind.Minted answers.
func TestRewriteFixture_ReportsAMintedCredential(t *testing.T) {
	t.Run("a stored credential is not reported", func(t *testing.T) {
		got, err := steelthread.RewriteFixture(liveFixture(t, nil))
		require.NoError(t, err)
		assert.Empty(t, got.MintedCredentials,
			"the base fixture's credential is type=static, which resolves from the placeholder Secret")
	})

	t.Run("a minted credential is reported, named and typed", func(t *testing.T) {
		got, err := steelthread.RewriteFixture(liveFixture(t, withMintedCredential))
		require.NoError(t, err)
		require.Len(t, got.MintedCredentials, 1)
		assert.Equal(t, "demoforge-identity/demoforge-app (githubApp)", got.MintedCredentials[0].Label,
			"the finding has to name the identity, the credential and the type, or the reader "+
				"cannot tell which of several credentials is the unresolvable one")
		assert.Equal(t, "githubApp", got.MintedCredentials[0].Type,
			"the TYPE is carried beside the label because it, not the prose, is what decides "+
				"whether a replay harness can serve the credential at all")

		// The trap this exists to catch: the fixture still emits a placeholder
		// Secret beside it, so nothing in the emitted files looks wrong.
		assert.Contains(t, got.ShadowedSecrets, "demoforge-app-creds",
			"a minted credential still gets a placeholder Secret; that is exactly why the "+
				"replayed AgentIdentity goes Valid and the failure only shows at dispatch")
	})
}

// TestCapture_MintedCredentialFindingComesFromWhatWasEMITTED drives the whole
// pipeline, because the rewrite and the check are tested apart and neither sees
// the wire between them.
//
// Same shape as the skills wiring test, and for the same reason: a value the
// rewrite reports and the self-check reads is one assignment away from being
// dropped, and both halves keep passing when it is.
//
// The two subtests are the two directions of the SAME wire, and running both is
// what keeps either honest. A check wired to nothing reports no finding, which
// is indistinguishable from the served case; a check that ignored the served
// list reports one always, which is indistinguishable from the unserved case.
func TestCapture_MintedCredentialFindingComesFromWhatWasEMITTED(t *testing.T) {
	captureWithMintedCredential := func(t *testing.T) []steelthread.Finding {
		t.Helper()
		in := captureInput(t)
		in.Fixture = liveFixture(t, withMintedCredential)
		// The placeholder Secret this credential gets shadows a live one, so
		// the leak scan requires its value — otherwise the run reports
		// secret-check-skipped and this test would pass on the wrong finding.
		in.LiveSecrets = append(in.LiveSecrets,
			steelthread.LiveSecret{Name: "demoforge-app-creds", EmptyRead: true})

		_, findings, err := steelthread.Capture(syntheticRecords(t), in)
		require.NoError(t, err)
		return findings
	}

	t.Run("a type the harness serves is not refused", func(t *testing.T) {
		require.Contains(t, bt.StandInMintedCredentialTypes, "githubApp",
			"this subtest asserts the SERVED path; it is meaningless if the type is not on the list")

		for _, f := range captureWithMintedCredential(t) {
			assert.NotEqual(t, steelthread.CodeCredentialNotMintable, f.Code,
				"the replay harness stands a minter up for this type against its own fixture "+
					"provider, so the credential is minted for real and the capture has nothing to refuse")
		}
	})

	t.Run("a type nothing serves is refused, naming it", func(t *testing.T) {
		// The list is what the check reads, so emptying it is the one mutation
		// that separates "the wire carries the credential" from "the type
		// happens to be served". Restored immediately: it is a package var
		// every other test in this binary reads.
		prev := bt.StandInMintedCredentialTypes
		bt.StandInMintedCredentialTypes = nil
		t.Cleanup(func() { bt.StandInMintedCredentialTypes = prev })

		f := findByCode(t, captureWithMintedCredential(t), steelthread.CodeCredentialNotMintable)
		assert.Equal(t, steelthread.SeverityHard, f.Severity)
		assert.Contains(t, f.Message, "demoforge-identity/demoforge-app (githubApp)")
	})
}

// TestReplayChannel_AnswersWithWhatTheRewriteWillEmit pins the seam the CLI
// assembles the fixture's meta tools against.
//
// It matters that this is the SAME rewrite: a caller that predicted the
// fixture's channel some other way could agree with itself and disagree with
// 03-agent.yaml, and the check built on it would then pass against a channel
// the fixture never writes.
func TestReplayChannel_AnswersWithWhatTheRewriteWillEmit(t *testing.T) {
	t.Run("a conversational Channel comes back as the fake kind", func(t *testing.T) {
		ch, sec, err := steelthread.ReplayChannel(liveFixture(t, nil), "demo-chat")
		require.NoError(t, err)
		assert.Equal(t, "fake", ch.Spec.Kind,
			"this is the rewrite that destroys a channel-sourced meta tool, and the whole "+
				"reason the fixture's tool set has to be assembled a second time")
		assert.Nil(t, ch.Spec.Slack, "the live kind's config block goes with the kind")
		require.NotNil(t, sec, "the assembly reads the bound Secret; a nil one is not the same input")
		assert.Equal(t, "demo-chat-slack-creds", sec.Name)
	})

	t.Run("the trigger's own Channel keeps its kind", func(t *testing.T) {
		in := liveFixture(t, nil)
		in.TriggerChannel = "demo-hooks"

		ch, _, err := steelthread.ReplayChannel(in, "demo-hooks")
		require.NoError(t, err)
		assert.Equal(t, "demoforge", ch.Spec.Kind,
			"bt.Trigger signs with this Channel's own kind, so the rewrite exempts it — and a "+
				"prediction that rewrote it anyway would claim tools the replay really does offer")
	})

	t.Run("an unknown name is an error, never a zero Channel", func(t *testing.T) {
		_, _, err := steelthread.ReplayChannel(liveFixture(t, nil), "no-such-channel")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no-such-channel")
	})
}

// TestRewriteFixture_MintedCredentialsAreDeterministic keeps the field inside
// the byte-identical-recapture promise the rest of RewriteResult holds to.
func TestRewriteFixture_MintedCredentialsAreDeterministic(t *testing.T) {
	in := liveFixture(t, func(f *steelthread.FixtureInput) {
		withMintedCredential(f)
		f.Identity.Spec.Credentials = append(f.Identity.Spec.Credentials,
			spiceboxv1alpha1.AgentCredential{
				Name:      "aaa-other-app",
				Type:      "githubApp",
				GitHubApp: &spiceboxv1alpha1.GitHubAppCredentialSource{SecretRef: spiceboxv1alpha1.SecretRef{Name: "other-app-creds"}},
			})
	})

	first, err := steelthread.RewriteFixture(in)
	require.NoError(t, err)
	second, err := steelthread.RewriteFixture(in)
	require.NoError(t, err)

	assert.Equal(t, first.MintedCredentials, second.MintedCredentials)
	assert.Equal(t, []steelthread.MintedCredential{
		{Label: "demoforge-identity/aaa-other-app (githubApp)", Type: "githubApp"},
		{Label: "demoforge-identity/demoforge-app (githubApp)", Type: "githubApp"},
	}, first.MintedCredentials, "sorted, so a re-capture's diff is empty rather than reordered")
}
