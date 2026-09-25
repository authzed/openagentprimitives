package channelplan_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	k8sfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/wizardkeys"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/channelplan"
)

// The namespace and ConfigMap the planner reads the external base URL from.
// Spelled as LITERALS, not as the constants the production code uses: this is
// the cluster-side contract with webd and with `oap init --local`, and a
// comparison of a constant against itself would keep passing through a rename
// that stopped the planner ever finding the value.
const (
	platformNamespace = "agentprimitives-system"
	webdURLConfigMap  = "spicebox-webd-external-url"
	webdTrustedURLKey = "trusted-url"
)

// planScheme is the scheme both objects the planner reads live in: the Channel
// it looks up and the ConfigMap it reads the URL from.
func planScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

func planCluster(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return k8sfake.NewClientBuilder().WithScheme(planScheme(t)).WithObjects(objs...).Build()
}

// existingChannel is a Channel already in the namespace. kind and agentClass
// are parameters and not defaults, because B-R11 makes them load-bearing: a
// Channel of the right NAME is only ours when both also match, so a fixture
// that fixed them would make every wired/conflicting case indistinguishable.
func existingChannel(t *testing.T, namespace, name, kind, agentClass string) client.Object {
	t.Helper()
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:       kind,
			Role:       spiceboxv1alpha1.ChannelRoleInput,
			AgentClass: agentClass,
		},
	}
}

// externalURLConfigMap is the ConfigMap webd publishes its externally
// reachable origin in.
func externalURLConfigMap(t *testing.T, url string) client.Object {
	t.Helper()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: platformNamespace, Name: webdURLConfigMap},
	}
	if url != "" {
		cm.Data = map[string]string{webdTrustedURLKey: url}
	}
	return cm
}

// bundleDeclaring builds a bundle whose manifest declares one channel.
// It goes through the same YAML path bundleWith uses (lint_test.go), so the
// declaration the planner reads is one ParseManifest actually produced.
func bundleDeclaring(t *testing.T, kind, role, name string) *oap.Bundle {
	t.Helper()
	return bundleWith(t, fmt.Sprintf(`requires:
  channels:
    - kind: %s
      role: %s
      name: %s
`, kind, role, name))
}

func TestPlanChannels_MatchingExistingChannelIsReportedWired(t *testing.T) {
	c := planCluster(t, existingChannel(t, "default", "demo-agent-gh", "github", "demo-agent"))

	plans, err := channelplan.PlanChannels(context.Background(), c,
		bundleDeclaring(t, "github", "input", "demo-agent-gh"), "default", "demo-agent", "")
	require.NoError(t, err)
	require.Len(t, plans, 1)

	assert.True(t, plans[0].AlreadyWired, "re-running install must not recreate a wired channel")
	assert.True(t, plans[0].WiringKnown, "the namespace was read, so the answer is an observation")
}

// TestPlanChannels_AbsentChannelIsReportedUnwiredAndKnown is the other half of
// the pair above, and it is not redundant with it: it is what separates
// "checked, and there is nothing there" from the offline plan below, which
// reports the same AlreadyWired for an entirely different reason.
func TestPlanChannels_AbsentChannelIsReportedUnwiredAndKnown(t *testing.T) {
	// A Channel of a DIFFERENT name, so the lookup is proved to match on the
	// name rather than on "any Channel in the namespace".
	c := planCluster(t, existingChannel(t, "default", "some-other-channel", "github", "demo-agent"))

	plans, err := channelplan.PlanChannels(context.Background(), c,
		bundleDeclaring(t, "github", "input", "demo-agent-gh"), "default", "demo-agent", "")
	require.NoError(t, err)
	require.Len(t, plans, 1)

	assert.False(t, plans[0].AlreadyWired, "nothing of this name is in the namespace")
	assert.True(t, plans[0].WiringKnown, "and that is an answer, not a failure to look")
}

// TestPlanChannels_WiringIsMatchedPerChannelInDeclarationOrder pins two facts
// one test cannot separate if it declares a single channel: that the order the
// clients process channels in is the order the bundle declares them, and that
// each plan's AlreadyWired belongs to ITS OWN declaration rather than to
// whichever channel the namespace happened to contain.
func TestPlanChannels_WiringIsMatchedPerChannelInDeclarationOrder(t *testing.T) {
	b := bundleWith(t, `requires:
  channels:
    - kind: github
      role: input
      name: demo-agent-gh
    - kind: slack
      role: both
      name: demo-agent-slack
    - kind: bento
      role: input
      name: demo-agent-cron
`)
	// Only the MIDDLE one exists, so a plan that reported wiring positionally,
	// or reported it cluster-wide, produces a different answer than this.
	c := planCluster(t, existingChannel(t, "default", "demo-agent-slack", "slack", "demo-agent"))

	plans, err := channelplan.PlanChannels(context.Background(), c, b, "default", "demo-agent", "")
	require.NoError(t, err)
	require.Len(t, plans, 3)

	assert.Equal(t, []string{"demo-agent-gh", "demo-agent-slack", "demo-agent-cron"},
		[]string{plans[0].Required.Name, plans[1].Required.Name, plans[2].Required.Name},
		"declaration order is the order install processes them")
	assert.Equal(t, []bool{false, true, false},
		[]bool{plans[0].AlreadyWired, plans[1].AlreadyWired, plans[2].AlreadyWired},
		"only the channel that exists is wired")
}

func TestPlanChannels_SeedsNameClassAndExternalURL(t *testing.T) {
	c := planCluster(t, externalURLConfigMap(t, "https://ap.demo.test"))

	plans, err := channelplan.PlanChannels(context.Background(), c,
		bundleDeclaring(t, "github", "input", "demo-agent-gh"), "default", "demo-agent", "")
	require.NoError(t, err)
	require.Len(t, plans, 1)

	s := plans[0].Seeded
	assert.Equal(t, "demo-agent-gh", s[wizardkeys.KeyChannelName],
		"the bundle declares the name; the operator cannot get it wrong")
	assert.Equal(t, "demo-agent", s[wizardkeys.KeyAgentClass],
		"one bundle, one agent — nothing to choose")
	assert.Equal(t, "https://ap.demo.test", s[wizardkeys.KeyExternalBaseURL],
		"webd already published this; asking for it invites a typo")

	// Pre-seeding must not be silent: a value the operator never supplied,
	// applied without a word, is impossible to debug when it is wrong.
	assert.Contains(t, plans[0].SeededFrom[wizardkeys.KeyExternalBaseURL], webdURLConfigMap,
		"a value taken from a ConfigMap the operator never saw must say where it came from")
	assert.Contains(t, plans[0].SeededFrom[wizardkeys.KeyExternalBaseURL], webdTrustedURLKey,
		"and which key inside it, since the ConfigMap carries two origins")
	for _, key := range []string{wizardkeys.KeyChannelName, wizardkeys.KeyAgentClass} {
		assert.NotEmpty(t, plans[0].SeededFrom[key], "every seeded key needs a stated source, not just the cluster-read one")
	}
}

// TestPlanChannels_EverySeededKeyIsAQuestionTheKindAsks is the join the value
// assertions above cannot make: it drives a REAL kind's Inputs and requires
// every key the planner seeds to be a question that kind actually poses.
//
// It is what a literal answer key in a test cannot prove. The brief this task
// came from named three keys and two of them ("channel-name", "agent-class")
// do not exist; a test asserting those literals against a planner that emitted
// them would have been perfectly green and seeded nothing a wizard reads,
// because the driver suppresses a question by MATCHING ITS NAME.
func TestPlanChannels_EverySeededKeyIsAQuestionTheKindAsks(t *testing.T) {
	c := planCluster(t,
		externalURLConfigMap(t, "https://ap.demo.test"),
		&spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "demo-agent"}},
	)
	ctx := context.Background()

	plans, err := channelplan.PlanChannels(ctx, c,
		bundleDeclaring(t, "github", "input", "demo-agent-gh"), "default", "demo-agent", "")
	require.NoError(t, err)
	require.Len(t, plans, 1)
	require.Len(t, plans[0].Seeded, 3, "all three seeds must be present, or the subset check below is vacuous")

	kind, ok := registry.Get("github")
	require.True(t, ok, "the test binary must have the github kind linked")
	qs, err := kind.Wizard().Inputs(ctx, channelkinds.WizardInput{
		Namespace: "default", K8s: c, Seeded: plans[0].Seeded,
	})
	require.NoError(t, err)

	asked := make(map[string]bool, len(qs))
	for _, q := range qs {
		asked[q.Name] = true
	}
	for key := range plans[0].Seeded {
		assert.True(t, asked[key],
			"the planner seeded %q, which no question of this kind is named after — nothing would ever read it", key)
	}
}

// TestPlanChannels_RoleIsCarriedOnTheDeclarationAndNeverSeeded pins the one
// thing in this task's brief that had to be dropped: NO kind's wizard asks for
// a role, so a seeded "role" would be an answer to nothing. The role travels
// on Required and reaches the Channel when the caller applies it.
func TestPlanChannels_RoleIsCarriedOnTheDeclarationAndNeverSeeded(t *testing.T) {
	plans, err := channelplan.PlanChannels(context.Background(), planCluster(t),
		bundleDeclaring(t, "github", "input", "demo-agent-gh"), "default", "demo-agent", "")
	require.NoError(t, err)
	require.Len(t, plans, 1)

	assert.Equal(t, spiceboxv1alpha1.ChannelRoleInput, plans[0].Required.Role,
		"the declared role reaches the caller on the plan")
	assert.NotContains(t, plans[0].Seeded, "role", "no wizard asks for a role, so seeding one answers nothing")
}

func TestPlanChannels_AbsentExternalURLIsNotSeeded(t *testing.T) {
	// No ConfigMap: the question must be asked, not seeded with "".
	c := planCluster(t)

	plans, err := channelplan.PlanChannels(context.Background(), c,
		bundleDeclaring(t, "github", "input", "demo-agent-gh"), "default", "demo-agent", "")
	require.NoError(t, err)
	require.Len(t, plans, 1)

	assert.NotContains(t, plans[0].Seeded, wizardkeys.KeyExternalBaseURL,
		"seeding an empty string would answer the question with nothing")
	assert.Contains(t, plans[0].NotSeeded[wizardkeys.KeyExternalBaseURL], "no ConfigMap",
		"the operator about to be asked for this by hand is told why it was not pre-filled")
	assert.Contains(t, plans[0].NotSeeded[wizardkeys.KeyExternalBaseURL], webdURLConfigMap,
		"and which ConfigMap would have carried it")
	assert.Contains(t, plans[0].Seeded, wizardkeys.KeyChannelName,
		"the seeds that do not depend on the cluster are unaffected")
}

// TestPlanChannels_BlankExternalURLKeyIsNotSeeded is the ConfigMap that EXISTS
// but has not been populated — the state `oap install` leaves behind before
// the webd task runs, and a distinct code path from the absent one above.
func TestPlanChannels_BlankExternalURLKeyIsNotSeeded(t *testing.T) {
	c := planCluster(t, externalURLConfigMap(t, ""))

	plans, err := channelplan.PlanChannels(context.Background(), c,
		bundleDeclaring(t, "github", "input", "demo-agent-gh"), "default", "demo-agent", "")
	require.NoError(t, err)
	require.Len(t, plans, 1)

	assert.NotContains(t, plans[0].Seeded, wizardkeys.KeyExternalBaseURL,
		"an unpopulated ConfigMap is not a URL")
	assert.Contains(t, plans[0].NotSeeded[wizardkeys.KeyExternalBaseURL], "carries no",
		"and the operator is told the ConfigMap exists but is empty, not that it is missing")
}

// TestPlanChannels_ExternalURLTrailingSlashIsTrimmed matters because callers
// concatenate a path onto this value: a base ending in "/" produces a webhook
// URL containing "//", which the receiving router answers with a 404 instead
// of a delivery.
func TestPlanChannels_ExternalURLTrailingSlashIsTrimmed(t *testing.T) {
	c := planCluster(t, externalURLConfigMap(t, "https://ap.demo.test/"))

	plans, err := channelplan.PlanChannels(context.Background(), c,
		bundleDeclaring(t, "github", "input", "demo-agent-gh"), "default", "demo-agent", "")
	require.NoError(t, err)
	require.Len(t, plans, 1)

	assert.Equal(t, "https://ap.demo.test", plans[0].Seeded[wizardkeys.KeyExternalBaseURL])
}

// TestPlanChannels_NilClientPlansEverythingAndClaimsNothing is the offline
// plan: `oap agent install --apply=false` with no cluster to ask.
//
// The distinction it exists to hold is WiringKnown. Reporting AlreadyWired
// false is right for a preview; reporting it as though the namespace had been
// checked is what would have a later, applying run create a Channel that is
// already there.
func TestPlanChannels_NilClientPlansEverythingAndClaimsNothing(t *testing.T) {
	var c client.Client // a genuine nil interface, not a typed-nil pointer

	plans, err := channelplan.PlanChannels(context.Background(), c,
		bundleDeclaring(t, "github", "input", "demo-agent-gh"), "default", "demo-agent", "")
	require.NoError(t, err, "an offline plan is a legitimate mode, not a failure")
	require.Len(t, plans, 1, "every declaration is still planned")

	assert.False(t, plans[0].AlreadyWired)
	assert.False(t, plans[0].WiringKnown,
		"nothing was checked, so 'not wired' is the absence of an answer and must say so")
	assert.Equal(t, "demo-agent-gh", plans[0].Seeded[wizardkeys.KeyChannelName],
		"a seed that does not need a cluster is still supplied offline")
	assert.Equal(t, "demo-agent", plans[0].Seeded[wizardkeys.KeyAgentClass])
	assert.NotContains(t, plans[0].Seeded, wizardkeys.KeyExternalBaseURL,
		"there was no cluster to read it from")
	assert.Contains(t, plans[0].NotSeeded[wizardkeys.KeyExternalBaseURL], "offline",
		"and the preview says the value was not fetched, rather than implying the cluster has none")
}

// TestPlanChannels_UnreadableChannelIsAnErrorNotAnUnwiredPlan is the
// difference between "there is no Channel" and "we could not find out".
// Folding the second into the first has install create a Channel that may
// already exist.
func TestPlanChannels_UnreadableChannelIsAnErrorNotAnUnwiredPlan(t *testing.T) {
	wantErr := errors.New("transient api failure")
	c := k8sfake.NewClientBuilder().WithScheme(planScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, isChannel := obj.(*spiceboxv1alpha1.Channel); isChannel {
				return wantErr
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()

	plans, err := channelplan.PlanChannels(context.Background(), c,
		bundleDeclaring(t, "github", "input", "demo-agent-gh"), "default", "demo-agent", "")
	require.Error(t, err, "an unreadable namespace must not read as an unwired channel")
	assert.ErrorIs(t, err, wantErr, "the underlying failure must reach the caller, not be replaced")
	assert.Contains(t, err.Error(), "demo-agent-gh", "and must name the Channel it could not read")
	assert.Nil(t, plans, "a plan built on an unknown is not a plan")
}

// refuseConfigMapReads is a client that fails every ConfigMap Get with err and
// serves everything else normally, so a test can prove what an unreadable
// ConfigMap does WITHOUT also disabling the Channel lookup.
func refuseConfigMapReads(t *testing.T, err error) client.Client {
	t.Helper()
	return k8sfake.NewClientBuilder().WithScheme(planScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, isConfigMap := obj.(*corev1.ConfigMap); isConfigMap {
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
}

// TestPlanChannels_UnreadableConfigMapDegradesAndSaysWhy — RULING B-R7.
//
// The URL is a SEED: an optimisation whose absence is already handled, because
// the question simply gets asked. Failing the whole plan on it would stop an
// operator with namespace-scoped RBAC, who cannot get configmaps in the
// platform namespace, from installing a bundle whose channels never wanted the
// URL — a real operator locked out by a value they did not need.
//
// So it degrades. What makes that not-swallowing is the prose, which must
// carry the API's own words: "forbidden" is the actionable half, and a generic
// "could not read it" would leave the operator no better off than silence.
func TestPlanChannels_UnreadableConfigMapDegradesAndSaysWhy(t *testing.T) {
	c := refuseConfigMapReads(t, errors.New("configmaps \"spicebox-webd-external-url\" is forbidden"))

	plans, err := channelplan.PlanChannels(context.Background(), c,
		bundleDeclaring(t, "github", "input", "demo-agent-gh"), "default", "demo-agent", "")
	require.NoError(t, err, "a seed this install could not read must not stop the install")
	require.Len(t, plans, 1)

	assert.NotContains(t, plans[0].Seeded, wizardkeys.KeyExternalBaseURL,
		"a value we failed to read is not a value")
	why := plans[0].NotSeeded[wizardkeys.KeyExternalBaseURL]
	assert.Contains(t, why, "forbidden",
		"the API's own words are the actionable half and must survive to the operator")
	assert.Contains(t, why, webdURLConfigMap, "and the message must name the ConfigMap it could not read")

	// The rest of the plan is unaffected — this is a degrade, not a bail-out.
	assert.Equal(t, "demo-agent-gh", plans[0].Seeded[wizardkeys.KeyChannelName])
	assert.True(t, plans[0].WiringKnown, "the Channel lookup still happened")
}

// TestPlanChannels_TheFourReasonsForNoExternalURLAreDistinct is the other half
// of B-R7: "there is no such ConfigMap" and "I was not allowed to look" are
// different things to tell an operator, and a plan that rendered them with one
// sentence would send someone to fix the wrong thing.
//
// Asserting they are pairwise distinct catches a collapse that per-case
// substring assertions cannot: three of these could drift into sharing a
// sentence while each still contained the substring its own test looks for.
func TestPlanChannels_TheFourReasonsForNoExternalURLAreDistinct(t *testing.T) {
	var offline client.Client
	cases := map[string]client.Client{
		"offline (nil client)":    offline,
		"no ConfigMap at all":     planCluster(t),
		"ConfigMap with no value": planCluster(t, externalURLConfigMap(t, "")),
		"ConfigMap unreadable":    refuseConfigMapReads(t, errors.New("is forbidden")),
	}

	seen := map[string]string{} // reason → the case that produced it
	for name, c := range cases {
		plans, err := channelplan.PlanChannels(context.Background(), c,
			bundleDeclaring(t, "github", "input", "demo-agent-gh"), "default", "demo-agent", "")
		require.NoError(t, err, name)
		require.Len(t, plans, 1, name)

		why := plans[0].NotSeeded[wizardkeys.KeyExternalBaseURL]
		require.NotEmpty(t, why, "%s: every not-seeded reason must be stated, or there is nothing to distinguish", name)
		if other, dup := seen[why]; dup {
			t.Errorf("%q and %q report the same reason %q; an operator cannot tell them apart", name, other, why)
		}
		seen[why] = name
	}
}

// TestPlanChannels_EmptyAgentClassSeedsNothing: the caller has not resolved
// the install's AgentClass name. Seeding "" would answer the binding question
// with nothing — the same rule the external URL follows — and the wizard's own
// AgentClass listing is the correct fallback.
func TestPlanChannels_EmptyAgentClassSeedsNothing(t *testing.T) {
	plans, err := channelplan.PlanChannels(context.Background(), planCluster(t),
		bundleDeclaring(t, "github", "input", "demo-agent-gh"), "default", "", "")
	require.NoError(t, err)
	require.Len(t, plans, 1)

	assert.NotContains(t, plans[0].Seeded, wizardkeys.KeyAgentClass)
	assert.NotContains(t, plans[0].SeededFrom, wizardkeys.KeyAgentClass)
	assert.Contains(t, plans[0].Seeded, wizardkeys.KeyChannelName, "the other seeds are unaffected")
}

// TestPlanChannels_NamelessDeclarationIsUnknownNotUnwired: a declaration with
// no name is an authoring error LintRequiredChannels reports. The planner must
// neither crash on it nor claim the namespace was checked — there is nothing
// to look a Channel up by.
func TestPlanChannels_NamelessDeclarationIsUnknownNotUnwired(t *testing.T) {
	b := bundleWith(t, `requires:
  channels:
    - kind: github
      role: input
`)
	plans, err := channelplan.PlanChannels(context.Background(), planCluster(t), b, "default", "demo-agent", "")
	require.NoError(t, err, "planning is not the lint; a nameless entry is reported there, not fatal here")
	require.Len(t, plans, 1)

	assert.False(t, plans[0].WiringKnown, "there was no name to look one up by")
	assert.False(t, plans[0].AlreadyWired)
	assert.NotContains(t, plans[0].Seeded, wizardkeys.KeyChannelName, "there is no name to seed")
}

func TestPlanChannels_NoDeclarationsIsAnEmptyPlanNotAnError(t *testing.T) {
	plans, err := channelplan.PlanChannels(context.Background(), planCluster(t),
		bundleWith(t, ""), "default", "demo-agent", "")
	require.NoError(t, err, "a bundle that declares no channel is the common case, not a failure")
	assert.Empty(t, plans)
}

func TestPlanChannels_MalformedBundleIsAnError(t *testing.T) {
	cases := []struct {
		name      string
		bundle    *oap.Bundle
		errSubstr string
	}{
		{name: "nil bundle: refused rather than dereferenced", bundle: nil, errSubstr: "nil bundle"},
		{name: "a bundle with no manifest: refused, since the declaration lives there", bundle: &oap.Bundle{}, errSubstr: "no manifest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plans, err := channelplan.PlanChannels(context.Background(), planCluster(t), tc.bundle, "default", "demo-agent", "")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errSubstr)
			assert.Nil(t, plans)
		})
	}
}

// TestPlanChannels_SeededFromCoversSeededAndNotSeededIsDisjoint — RULING B-R10.
//
// The split only buys anything if the two maps are actually disjoint and
// SeededFrom actually covers Seeded exactly. Documenting that would be the
// weak version; this asserts it across every shape a real run lands in, so a
// consumer can render each map without consulting the other.
//
// The key-SET equality is what a per-key assertion misses: a stray entry in
// SeededFrom for a key that was never seeded is exactly the mislabelling the
// split exists to prevent, and no "did this key get a source" check sees it.
func TestPlanChannels_SeededFromCoversSeededAndNotSeededIsDisjoint(t *testing.T) {
	var offline client.Client
	clusters := map[string]client.Client{
		"URL published":      planCluster(t, externalURLConfigMap(t, "https://ap.demo.test")),
		"no ConfigMap":       planCluster(t),
		"ConfigMap empty":    planCluster(t, externalURLConfigMap(t, "")),
		"read refused":       refuseConfigMapReads(t, errors.New("is forbidden")),
		"offline, no client": offline,
	}

	for name, c := range clusters {
		t.Run(name, func(t *testing.T) {
			plans, err := channelplan.PlanChannels(context.Background(), c,
				bundleDeclaring(t, "github", "input", "demo-agent-gh"), "default", "demo-agent", "")
			require.NoError(t, err)
			require.Len(t, plans, 1)
			p := plans[0]

			require.NotNil(t, p.Seeded)
			require.NotNil(t, p.SeededFrom)
			require.NotNil(t, p.NotSeeded, "all three maps are allocated, so a consumer never writes into nil")

			assert.Equal(t, slices.Sorted(maps.Keys(p.Seeded)), slices.Sorted(maps.Keys(p.SeededFrom)),
				"SeededFrom must carry a source for every seeded key and for no other key")
			for key := range p.NotSeeded {
				assert.NotContains(t, p.Seeded, key, "a key cannot be both seeded and explained as absent")
				assert.NotContains(t, p.SeededFrom, key, "and a reason must never be filed as a provenance note")
			}
		})
	}
}

// TestPlanChannels_NamedInstallOfAChannelDeclaringBundleIsRefused — RULING B-R9.
//
// `instance.Rename` prefixes every bundled CR, but a declared channel is not a
// CR, so two named installs declare the SAME channel name: the second finds the
// first's Channel, reports AlreadyWired, and installs an agent bound to
// nothing, because a Channel serves exactly one AgentClass. Nothing errors and
// nothing logs — instance B's work is answered by instance A.
//
// The refusal converts that silent wrong outcome into a loud one. It lives in
// the planner because both `oap agent install` and admind call it, and one
// caller being missed is how this class survives.
func TestPlanChannels_NamedInstallOfAChannelDeclaringBundleIsRefused(t *testing.T) {
	plans, err := channelplan.PlanChannels(context.Background(), planCluster(t),
		bundleDeclaring(t, "github", "input", "demo-agent-gh"), "default", "demo-agent", "my-bot")
	require.Error(t, err, "a silent cross-binding must become a refusal")
	assert.Nil(t, plans, "nothing is planned for a combination that cannot be installed")

	msg := err.Error()
	assert.Contains(t, msg, "my-bot", "the operator must see the flag value they passed")
	assert.Contains(t, msg, "demo-agent-gh", "and which declared channel it collides with")
	assert.Contains(t, msg, "oap channel create", "and the manual route out of the refusal")
	assert.Contains(t, msg, "namespace", "and that a namespace per instance is the other way")
}

// TestPlanChannels_NamedInstallRefusalNamesEveryDeclaredChannel: the operator
// fixes the bundle or the command once, so being told about one of three
// collisions would send them round the loop twice more.
func TestPlanChannels_NamedInstallRefusalNamesEveryDeclaredChannel(t *testing.T) {
	b := bundleWith(t, `requires:
  channels:
    - kind: github
      role: input
      name: demo-agent-gh
    - kind: slack
      role: both
      name: demo-agent-slack
`)
	_, err := channelplan.PlanChannels(context.Background(), planCluster(t), b, "default", "demo-agent", "my-bot")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "demo-agent-gh")
	assert.Contains(t, err.Error(), "demo-agent-slack")
}

// TestPlanChannels_NamedInstallOfABundleDeclaringNoChannelIsAllowed is the
// control that keeps the refusal from being a blanket ban on --name. A bundle
// with no declaration has nothing that could collide, and `--name` is a
// supported install shape for it.
func TestPlanChannels_NamedInstallOfABundleDeclaringNoChannelIsAllowed(t *testing.T) {
	plans, err := channelplan.PlanChannels(context.Background(), planCluster(t),
		bundleWith(t, ""), "default", "demo-agent", "my-bot")
	require.NoError(t, err, "--name is refused only for the combination that breaks, not on its own")
	assert.Empty(t, plans)
}

// TestPlanChannels_NamedInstallIsRefusedBeforeAnyClusterRead pins the ORDER.
// Planning a combination that cannot be installed should not first go and read
// the cluster, and a refusal that arrived only after an RBAC-blocked read would
// surface as the wrong error entirely.
//
// The client fails this test if it is touched at all, which is a stronger claim
// than any assertion about the message.
func TestPlanChannels_NamedInstallIsRefusedBeforeAnyClusterRead(t *testing.T) {
	touched := false
	c := k8sfake.NewClientBuilder().WithScheme(planScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			touched = true
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()

	_, err := channelplan.PlanChannels(context.Background(), c,
		bundleDeclaring(t, "github", "input", "demo-agent-gh"), "default", "demo-agent", "my-bot")
	require.Error(t, err)
	assert.False(t, touched, "the refusal must land before the planner reads anything from the cluster")
}

// TestPlanChannels_AChannelOfTheNameThatIsNotOursIsAConflictNotWired —
// RULING B-R11.
//
// A name match is not "already wired". A bundle upgrade that changes a declared
// channel's kind, or an operator who hand-created a Channel of that name for
// another agent, would otherwise get AlreadyWired, nothing created, every CR
// healthy — and an unreachable agent. That is the same silent cross-binding
// refuseNamedInstall refuses at length; reaching it without a name prefix would
// make that refusal arbitrary.
//
// Each row changes exactly ONE of the two things compared, so a check that read
// only the kind, or only the AgentClass, leaves a row green.
func TestPlanChannels_AChannelOfTheNameThatIsNotOursIsAConflictNotWired(t *testing.T) {
	cases := []struct {
		name         string
		existing     client.Object
		declaredKind string
		agentClass   string
		wantKind     string
		wantClass    string
		wantReason   []string
	}{
		{
			name:         "a different kind under the same name: conflict naming both kinds",
			existing:     existingChannel(t, "default", "demo-agent-gh", "github", "demo-agent"),
			declaredKind: "slack",
			agentClass:   "demo-agent",
			wantKind:     "github",
			wantClass:    "demo-agent",
			wantReason:   []string{"demo-agent-gh", `"github"`, `"slack"`},
		},
		{
			name:         "the same kind bound to another agent: conflict naming both agents",
			existing:     existingChannel(t, "default", "demo-agent-gh", "github", "other-agent"),
			declaredKind: "github",
			agentClass:   "demo-agent",
			wantKind:     "github",
			wantClass:    "other-agent",
			wantReason:   []string{"demo-agent-gh", `"other-agent"`, `"demo-agent"`},
		},
		{
			name:         "a monitoring channel holding the name: conflict, and the message says it binds to no agent",
			existing:     existingChannel(t, "default", "demo-agent-gh", "github", ""),
			declaredKind: "github",
			agentClass:   "demo-agent",
			wantKind:     "github",
			wantClass:    "",
			wantReason:   []string{"demo-agent-gh", "no agent", "monitoring"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plans, err := channelplan.PlanChannels(context.Background(), planCluster(t, tc.existing),
				bundleDeclaring(t, tc.declaredKind, "input", "demo-agent-gh"), "default", tc.agentClass, "")
			require.NoError(t, err, "a conflict is reported on the plan, not raised — Task 4 decides")
			require.Len(t, plans, 1)

			require.NotNil(t, plans[0].Conflict, "a Channel of this name exists and is not ours")
			assert.False(t, plans[0].AlreadyWired,
				"reporting it as wired is the silent cross-binding; false makes a careless consumer "+
					"attempt a create the apiserver refuses by name, which is loud")
			assert.True(t, plans[0].WiringKnown, "the namespace was read, so this IS an observation")

			assert.Equal(t, tc.wantKind, plans[0].Conflict.Kind,
				"the observed kind is carried so a client need not re-read the object")
			assert.Equal(t, tc.wantClass, plans[0].Conflict.AgentClass)
			for _, want := range tc.wantReason {
				assert.Contains(t, plans[0].Conflict.Reason, want)
			}
		})
	}
}

// TestPlanChannels_AMatchingChannelIsNotAConflict is the control for the rows
// above: with both facts agreeing, the same code path must report plain wired.
// Without it a conflictWith that returned non-nil unconditionally would satisfy
// every assertion in that table.
func TestPlanChannels_AMatchingChannelIsNotAConflict(t *testing.T) {
	plans, err := channelplan.PlanChannels(context.Background(),
		planCluster(t, existingChannel(t, "default", "demo-agent-gh", "github", "demo-agent")),
		bundleDeclaring(t, "github", "input", "demo-agent-gh"), "default", "demo-agent", "")
	require.NoError(t, err)
	require.Len(t, plans, 1)

	assert.Nil(t, plans[0].Conflict, "same name, same kind, same agent — this Channel is ours")
	assert.True(t, plans[0].AlreadyWired)
}

// TestPlanChannels_AnUnresolvedAgentClassJudgesOnlyTheKind: a caller that has
// not resolved its AgentClass cannot judge the binding, and inventing a
// mismatch from the empty string would report a conflict against every Channel
// in the namespace — turning an unremarkable offline-planned re-run into a
// refusal. The kind is still compared, because the declaration carries it.
func TestPlanChannels_AnUnresolvedAgentClassJudgesOnlyTheKind(t *testing.T) {
	c := planCluster(t, existingChannel(t, "default", "demo-agent-gh", "github", "some-other-agent"))

	plans, err := channelplan.PlanChannels(context.Background(), c,
		bundleDeclaring(t, "github", "input", "demo-agent-gh"), "default", "", "")
	require.NoError(t, err)
	require.Len(t, plans, 1)
	assert.Nil(t, plans[0].Conflict, "with no AgentClass to compare, the binding is not judged")
	assert.True(t, plans[0].AlreadyWired)

	// …but the kind still is.
	plans, err = channelplan.PlanChannels(context.Background(), c,
		bundleDeclaring(t, "slack", "both", "demo-agent-gh"), "default", "", "")
	require.NoError(t, err)
	require.Len(t, plans, 1)
	require.NotNil(t, plans[0].Conflict, "the declared kind is always comparable")
	assert.False(t, plans[0].AlreadyWired)
}

// TestPlanChannels_AnEmptyNamespaceIsUnknownNotAbsent — minor 3.
//
// A namespaced Get with no namespace segment 404s against a real apiserver, so
// IsNotFound fires there too and the planner would report an observation nobody
// made. This was the one path that escaped the tri-state.
func TestPlanChannels_AnEmptyNamespaceIsUnknownNotAbsent(t *testing.T) {
	touched := false
	c := k8sfake.NewClientBuilder().WithScheme(planScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, isChannel := obj.(*spiceboxv1alpha1.Channel); isChannel {
				touched = true
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()

	plans, err := channelplan.PlanChannels(context.Background(), c,
		bundleDeclaring(t, "github", "input", "demo-agent-gh"), "", "demo-agent", "")
	require.NoError(t, err)
	require.Len(t, plans, 1)

	assert.False(t, plans[0].WiringKnown,
		"nothing was asked in any real namespace, so 'not wired' is the absence of an answer")
	assert.False(t, plans[0].AlreadyWired)
	assert.False(t, touched, "and the pointless Get is not made at all")
}

// TestPlanChannels_ADeclaredNameIsTrimmedOnceForBothSeedAndLookup: the seeded
// name and the name looked up must be the same string. They were not — seed
// trimmed and the lookup did not — so a declaration with a trailing space
// reported a Channel that is right there as absent.
func TestPlanChannels_ADeclaredNameIsTrimmedOnceForBothSeedAndLookup(t *testing.T) {
	c := planCluster(t, existingChannel(t, "default", "demo-agent-gh", "github", "demo-agent"))

	plans, err := channelplan.PlanChannels(context.Background(), c,
		bundleDeclaring(t, "github", "input", `"demo-agent-gh "`), "default", "demo-agent", "")
	require.NoError(t, err)
	require.Len(t, plans, 1)

	assert.Equal(t, "demo-agent-gh", plans[0].Seeded[wizardkeys.KeyChannelName])
	assert.True(t, plans[0].AlreadyWired,
		"the trimmed name is what was seeded, so it must also be what was looked up")
}

// TestPlanChannels_AWhitespaceOnlyNameIsRefusedAsANamedInstall: install gates
// its rename on a bare `opts.Name != ""`, so `--name "   "` DOES prefix every
// CR. A planner that trimmed before deciding whether to refuse would wave
// through exactly the combination the refusal exists for. One predicate, one
// fact.
func TestPlanChannels_AWhitespaceOnlyNameIsRefusedAsANamedInstall(t *testing.T) {
	_, err := channelplan.PlanChannels(context.Background(), planCluster(t),
		bundleDeclaring(t, "github", "input", "demo-agent-gh"), "default", "demo-agent", "   ")
	require.Error(t, err, "install would rename on this value, so the planner must refuse on it too")
}
