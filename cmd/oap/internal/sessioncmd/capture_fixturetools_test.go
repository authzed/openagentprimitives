package sessioncmd

import (
	"bytes"
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	channelregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// chatFixture is a session on a real conversational channel kind, with the
// fixture manifests the capture would rewrite.
//
// slack rather than fake, because the whole subject is what the REWRITE takes
// away: a fixture already on the fake kind has nothing to lose and would make
// every case below vacuous.
func chatFixture(t *testing.T) (
	steelthread.FixtureInput, *spiceboxv1alpha1.AgentSession, *corev1.Secret,
) {
	t.Helper()
	const ns = "default"

	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: ns},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			// Granted so the prediction has a reader-gated tool in it at all:
			// fetch_artifact is what the first live capture got wrong, and with
			// no grant the case could not tell a correct answer from an
			// accidental one.
			Capabilities: map[string]apiextensionsv1.JSON{"attachments": {Raw: []byte(`{}`)}},
		},
	}
	chat := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-chat", Namespace: ns},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "slack",
			Role:           spiceboxv1alpha1.ChannelRoleBoth,
			AgentClass:     "demo-agent",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "demo-chat-creds"},
		},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-chat-creds", Namespace: ns},
		Data:       map[string][]byte{"bot-token": []byte("xoxb-fixture-only")},
	}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: ns},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class: "demo-agent",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "demo-chat", Kind: "slack", Capabilities: []string{"text", "markdown"},
			},
		},
	}
	fixture := steelthread.FixtureInput{
		Class:    class,
		Channels: []*spiceboxv1alpha1.Channel{chat},
	}
	return fixture, sess, sec
}

// triggerFixture is a github-TRIGGERED session: the bound Channel is the
// webhook source, and bt.Trigger's contract exempts it from the rewrite, so the
// replay binds to the same kind the run did.
//
// grants is the class's spec.capabilities, so a case can turn a capability on
// and watch what it contributes reach the prediction.
func triggerFixture(t *testing.T, grants map[string]apiextensionsv1.JSON) (
	steelthread.FixtureInput, *spiceboxv1alpha1.AgentSession, client.Client,
) {
	t.Helper()
	const ns = "default"

	class := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: ns},
		Spec:       spiceboxv1alpha1.AgentClassSpec{Capabilities: grants},
	}
	hooks := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-hooks", Namespace: ns},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "github",
			Role:           spiceboxv1alpha1.ChannelRoleInput,
			AgentClass:     "demo-agent",
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "demo-hooks-creds"},
		},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-hooks-creds", Namespace: ns},
		Data:       map[string][]byte{"webhook-secret": []byte("fixture-only")},
	}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: ns},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:        "demo-agent",
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "demo-hooks", Kind: "github"},
		},
	}
	return steelthread.FixtureInput{
		Class:          class,
		Channels:       []*spiceboxv1alpha1.Channel{hooks},
		TriggerChannel: "demo-hooks",
	}, sess, fakeChannelClient(t, hooks, sec)
}

// surfaceSplit runs the prediction and splits it the way the self-check reads
// it: the tools marked as reaching a provider's own surface, and the rest.
// External is sorted so a case can compare it whole.
func surfaceSplit(t *testing.T, fixture steelthread.FixtureInput, sess *spiceboxv1alpha1.AgentSession, cli client.Client) (
	external, ordinary []string,
) {
	t.Helper()
	var warn bytes.Buffer
	ctx := context.Background()
	got := fixtureMetaTools(ctx, fixture, fixture.Class, sess, resolveLiveChannel(ctx, cli, sess, &warn), nil, &warn)
	require.NotEmpty(t, got.Tools, "the prediction must be made at all before it can be split")
	for _, tl := range got.Tools {
		if tl.ExternalSurface {
			external = append(external, tl.Name)
		} else {
			ordinary = append(ordinary, tl.Name)
		}
	}
	slices.Sort(external)
	return external, ordinary
}

func names(tools []steelthread.FixtureTool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

// TestFixtureMetaTools_SeedsBackWhatTheChannelRewriteTakesAway is the whole
// mention story, driven through the production functions.
//
// The live session was OFFERED lookup_user_for_mention because slack advertises
// mention lookups. The capture rewrites that Channel to kind=fake, which by
// default advertises none — so the replay would be offered a smaller set than
// the recorded catalog pins, and the bundle would emit clean and fail inside the
// suite. The fix is tier 2: seed the fake kind's directory with what the live
// kind advertised, and the OFFER comes back.
//
// The three assertions are one claim each and all three are needed. That the
// tool is back is the fix; that it is MARKED StoodIn is what stops a bundle
// resting on its reply; that the seed carries the live kind's own lookups is
// what keeps the offer honest rather than invented.
func TestFixtureMetaTools_SeedsBackWhatTheChannelRewriteTakesAway(t *testing.T) {
	fixture, sess, sec := chatFixture(t)
	cli := fakeChannelClient(t, fixture.Channels[0], sec)
	var warn bytes.Buffer
	ctx := context.Background()

	live := resolveLiveChannel(ctx, cli, sess, &warn)
	require.NoError(t, live.Err, "the live Channel must resolve, or both lists are short for the same reason")

	liveNames := assembleMetaTools(ctx, fixture.Class, sess, sess.Spec.InputChannel, live, nil, &warn)
	got := fixtureMetaTools(ctx, fixture, fixture.Class, sess, live, nil, &warn)

	require.Contains(t, liveNames, "lookup_user_for_mention",
		"the premise: slack advertises mention lookups, so the live run was offered the tool")
	assert.Contains(t, names(got.Tools), "lookup_user_for_mention",
		"the seeded stand-in puts the OFFER back; without it the recorded catalog names a tool "+
			"the replay cannot produce and the bundle dies inside someone else's test run")

	require.NotNil(t, got.Mentions, "a restored offer has to be recorded, or the replay seeds nothing")
	assert.Equal(t, []string{"email", "name", "any"}, got.Mentions.Lookups,
		"the lookups come from the LIVE kind's own advertisement; a list written by the capture "+
			"would be its opinion about what a channel offers")
	assert.Empty(t, got.Mentions.Users,
		"a directory entry's provider id reaches a record only inside the tool's reply, which is "+
			"the stand-in kind's own rendering — so a CALL is refused rather than served a guess")

	assert.True(t, toolNamed(t, got.Tools, "lookup_user_for_mention").StoodIn,
		"the tool exists only because the seed put it back, and that is what makes a recorded "+
			"CALL to it refusable while a recorded OFFER of it is fine")

	// The negative half: the prediction is not simply empty, and the seed did
	// not sweep unrelated tools into StoodIn.
	assert.Contains(t, names(got.Tools), "agent_work_complete")
	assert.Contains(t, names(got.Tools), "respond_to_user")
	assert.False(t, toolNamed(t, got.Tools, "agent_work_complete").StoodIn,
		"a tool that was there before the seed must not be attributed to it")

	// fetch_artifact, specifically, and it is a regression the first live
	// capture caught: the tool exists whenever a RunnerEnv.ArtifactReader does,
	// and the replay driver wires one for every bundle. An assembly that left
	// the field nil predicted "the fixture will not offer fetch_artifact" about
	// a replay that certainly would — a hard finding on a session with nothing
	// wrong with it, which is the exact failure a hard check must not produce.
	assert.Contains(t, names(got.Tools), "fetch_artifact",
		"the prediction has to answer for what the REPLAY wires, not for what a laptop "+
			"running a capture happens to have")
}

// TestFixtureMetaTools_LeavesTheStandInOffAfterPredicting is small and is the
// one that protects everything else in this binary.
//
// The seed is process-wide state, so a prediction that left it on would widen
// the tool list of every capture and every bundle that ran afterwards — and each
// would then pass or fail for reasons nothing in its own input explains.
func TestFixtureMetaTools_LeavesTheStandInOffAfterPredicting(t *testing.T) {
	require.Empty(t, fakekind.MentionLookupsEnabled(),
		"a leak from another test in this process is itself the bug")

	fixture, sess, sec := chatFixture(t)
	cli := fakeChannelClient(t, fixture.Channels[0], sec)
	var warn bytes.Buffer
	ctx := context.Background()

	got := fixtureMetaTools(ctx, fixture, fixture.Class, sess, resolveLiveChannel(ctx, cli, sess, &warn), nil, &warn)
	require.NotNil(t, got.Mentions, "the premise: this fixture DOES seed a directory")

	assert.Empty(t, fakekind.MentionLookupsEnabled(),
		"the prediction must hand back exactly what it took")
}

// TestNeedsFakeDeliverySurfaces covers the THIRD thing the rewrite to kind=fake
// takes away, after a provider surface and a directory.
//
// respond_to_user hides its `attached` field behind an asset:* capability, so a
// captured run that delivered an artifact replays into our own code correctly
// refusing a capability the recorded channel really had — a refusal that reads
// as a product bug rather than as a fixture gap. bt.FakeDeliverySurfaces already
// existed as the opt-in a hand-authored bundle uses for exactly this; the
// capture only has to notice it is needed.
//
// Derived by comparing the two kinds' own Capabilities. A list written into the
// capture would be its opinion about what a transport can carry.
func TestNeedsFakeDeliverySurfaces(t *testing.T) {
	kindNamed := func(t *testing.T, name string) channelkinds.Kind {
		t.Helper()
		k, ok := channelregistry.Get(name)
		require.True(t, ok, "kind %q must be registered in this binary", name)
		return k
	}

	cases := []struct {
		name       string
		liveKind   string
		replayKind string
		want       bool
	}{
		{
			name:       "slack rewritten to fake loses its asset capabilities",
			liveKind:   "slack",
			replayKind: "fake",
			want:       true,
		},
		{
			name: "a trigger Channel keeps its own kind and loses nothing",
			// bt.Trigger's contract exempts the input Channel from the rewrite,
			// so both sides are the same kind and there is nothing to restore.
			liveKind:   "github",
			replayKind: "github",
			want:       false,
		},
		{
			name:       "a live kind that advertised no asset capability either",
			liveKind:   "fake",
			replayKind: "fake",
			want:       false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			live := boundChannel{Kind: kindNamed(t, tc.liveKind)}
			assert.Equal(t, tc.want, needsFakeDeliverySurfaces(live, kindNamed(t, tc.replayKind)))
		})
	}

	t.Run("an unresolved live kind claims nothing", func(t *testing.T) {
		assert.False(t, needsFakeDeliverySurfaces(boundChannel{}, kindNamed(t, "fake")),
			"with no live kind to compare against there is no honest answer, and turning a "+
				"capability on that the recorded channel may not have had would widen the "+
				"replayed tool set past what the run saw")
	})
}

// toolNamed returns the one prediction entry called name.
func toolNamed(t *testing.T, tools []steelthread.FixtureTool, name string) steelthread.FixtureTool {
	t.Helper()
	for _, tl := range tools {
		if tl.Name == name {
			return tl
		}
	}
	t.Fatalf("the prediction carries no tool named %q; it has %v", name, names(tools))
	return steelthread.FixtureTool{}
}

// TestFixtureMetaTools_MarksTheInputKindsOwnProviderTools pins the second fact
// the prediction carries.
//
// A github-triggered session keeps its input Channel's kind — bt.Trigger signs
// with it — so the trigger-status tools ARE offered at replay. They still cannot
// be replayed faithfully, because they answer on the provider's own surface.
//
// This pins the VALUES for a real github session. That the set follows the
// declaring capability rather than a list is pinned separately, by
// TestExternalSurfaceTools_FollowsTheDeclaringCapability.
func TestFixtureMetaTools_MarksTheInputKindsOwnProviderTools(t *testing.T) {
	fixture, sess, cli := triggerFixture(t, nil)

	external, ordinary := surfaceSplit(t, fixture, sess, cli)

	assert.Equal(t, []string{"claim_trigger_status", "conclude_trigger_status"}, external,
		"exactly the tools the declaring capability contributes for a github trigger")
	assert.Contains(t, ordinary, "agent_work_complete",
		"a tool no provider-surface capability contributed must not be swept up")
	assert.Contains(t, ordinary, "respond_to_user",
		"the reply the session owes its user goes out over a channel the fixture "+
			"DOES stand in for; marking it external refuses every triggered session")
}

// TestFixtureMetaTools_NeverGuessesWhenItCannotPredict pins the fail-closed
// direction. An empty list is what the self-check refuses on; a WRONG list would
// be worse than none, because it would report a real gap as clean.
func TestFixtureMetaTools_NeverGuessesWhenItCannotPredict(t *testing.T) {
	t.Run("a bound Channel that does not resolve yields no prediction", func(t *testing.T) {
		fixture, sess, _ := chatFixture(t)
		var warn bytes.Buffer
		ctx := context.Background()

		// No Channel object at all: the shape of a session whose Channel was
		// deleted after the run.
		live := resolveLiveChannel(ctx, fakeChannelClient(t), sess, &warn)
		require.Error(t, live.Err)

		assert.Nil(t, fixtureMetaTools(ctx, fixture, fixture.Class, sess, live, nil, &warn).Tools,
			"with no live kind to rewrite FROM there is no honest prediction, and the capture "+
				"must refuse rather than compare against a list it made up")
		assert.Contains(t, warn.String(), "demo-chat")
	})

	t.Run("a Channel the fixture never gathered is warned about, not guessed at", func(t *testing.T) {
		fixture, sess, sec := chatFixture(t)
		cli := fakeChannelClient(t, fixture.Channels[0], sec)
		// The manifests the capture gathered do not include the Channel the
		// session actually bound to.
		fixture.Channels = nil
		var warn bytes.Buffer
		ctx := context.Background()

		got := fixtureMetaTools(ctx, fixture, fixture.Class, sess, resolveLiveChannel(ctx, cli, sess, &warn), nil, &warn)
		assert.Nil(t, got.Tools)
		assert.Nil(t, got.Mentions, "a prediction that could not be made seeds nothing either")
		assert.Contains(t, warn.String(), "fixture")
	})

	t.Run("a session with no channel predicts the same set the live run had", func(t *testing.T) {
		fixture, sess, _ := chatFixture(t)
		sess.Spec.InputChannel = nil
		var warn bytes.Buffer
		ctx := context.Background()

		got := fixtureMetaTools(ctx, fixture, fixture.Class, sess, boundChannel{}, nil, &warn)
		require.NotEmpty(t, got.Tools, "a kubectl-started session still gets the always-on tools")
		assert.Contains(t, names(got.Tools), "agent_work_complete")
		assert.Nil(t, got.Mentions, "no binding means no directory was taken away, so none is seeded back")
		for _, tl := range got.Tools {
			assert.False(t, tl.ExternalSurface,
				"no binding means no kind to contribute a provider surface: %s", tl.Name)
			assert.False(t, tl.StoodIn,
				"and nothing was stood in for: %s", tl.Name)
		}
	})
}
