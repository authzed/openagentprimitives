package capability

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// This file is the before/after tool-set equivalence matrix for the
// capability-gated meta-tool refactor (Task 9A). It pins the exact set of
// tools an agent receives, across representative scenarios, and proves:
//
//  1. equivalence — granting all opt-in capabilities (memory, knowledge,
//     artifacts) reproduces the EXACT pre-refactor universal tool set for
//     that scenario (no default-on tool lost or renamed).
//  2. the intended delta — granting none removes ONLY the opt-in families
//     and nothing else.
//
// The "before" (legacy) sets below are transcribed verbatim from the task
// brief's scenario matrix (.superpowers/sdd/task-9A-brief.md), which in turn
// captures the pre-refactor internal/cmd/runner/main.go:466-643,1206-1248 behavior.

// optInFamilies is exactly the set that flips to opt-in under the refactor.
//
// record_observation is a POST-refactor addition (the resource-memory-write-
// path plan's Task 5) — it did not exist in the pre-refactor tree this file
// pins, so it is not part of Task 9A's "legacy" history. It is listed here,
// and folded into every "legacy" row below, because it is offered by the same
// opt-in memory capability as query_memory/search_memory: the equivalence
// property this file checks (grant-all reproduces the named set exactly,
// grant-none drops only the opt-in families) still has to hold with it
// included, or a future silent change to the memory capability's Offer would
// slip past this guard the same way the ones it was written for would have.
var optInFamilies = map[string]bool{
	"query_memory": true, "search_memory": true, "query_knowledge": true, "record_observation": true,
	"artifact_prepare": true, "artifact_await": true, "artifact_history": true, "artifact_offer_view": true,
	"update_view": true, "read_view": true,
}

func grantAllOptIns() map[string]apiextensionsv1.JSON {
	return map[string]apiextensionsv1.JSON{
		"memory":      {Raw: []byte(`{}`)},
		"knowledge":   {Raw: []byte(`{}`)},
		"artifacts":   {Raw: []byte(`{}`)},
		"update_view": {Raw: []byte(`{}`)},
		"read_view":   {Raw: []byte(`{}`)},
	}
}

func names(deps AssembleDeps) []string {
	got := Assemble(context.Background(), deps)
	out := make([]string, 0, len(got))
	for _, t := range got {
		out = append(out, t.Name())
	}
	sort.Strings(out)
	return out
}

func withoutOptIn(legacy []string) []string {
	out := make([]string, 0, len(legacy))
	for _, n := range legacy {
		if !optInFamilies[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// noopPublish/noopRequest are minimal non-nil NATS stand-ins so
// channel_interaction / core's channel-aware swap activate exactly as they
// would in production (nil-checked, never invoked — these tests only assert
// on the assembled tool NAMES, not on tool execution).
func noopPublish(context.Context, string, []byte) error { return nil }

// channelBaseFixture builds a channel-attached Binding + Session pair with
// the given RoutingMode and Capabilities, wired the same way
// internal/cmd/runner/main.go wires them (Binding == Session.Spec.InputChannel).
// OutputChannel is left nil, so SameChannelAs(nil) is true (output==input) —
// the no-leakage branch channelhistorygate.Offer needs for the mention_only
// row below.
func channelBaseFixture(routingMode string, capabilities []string) (*spiceboxv1alpha1.ChannelBinding, *spiceboxv1alpha1.AgentSession) {
	binding := &spiceboxv1alpha1.ChannelBinding{
		Name: "c1", Kind: "slack", RoutingMode: routingMode, Capabilities: capabilities,
	}
	sess := &spiceboxv1alpha1.AgentSession{
		Spec: spiceboxv1alpha1.AgentSessionSpec{InputChannel: binding},
	}
	return binding, sess
}

// baseChannelEnv is the common RunnerEnv wiring every channel-attached
// scenario needs so channel_interaction (respond_to_user/await_user_message/
// update_status) and core's channel-aware agent_work_complete swap activate.
// Memory/search/kg are left AVAILABLE and the artifact service is WIRED — the
// legacy set assumes they were present (production always wires the artifact
// service, so the artifacts capability's nil-service guard never fires there).
// resolveErr, when non-nil, makes mention_lookup and channel_history decline
// (SkipReason / quiet-inactive respectively) WITHOUT dereferencing a nil
// ResolvedChannel — this is how scenarios that don't want those two
// resolve-dependent tools opt out cleanly (a nil ResolvedChannel with a nil
// ResolveErr would panic inside channelhistorygate.Offer / mention_lookup's
// Offer, since production never produces that combination).
func baseChannelEnv(resolveErr error) RunnerEnv {
	return RunnerEnv{
		MemoryAvailable: true, SearchAvailable: true, KGAvailable: true,
		ChannelAttached: true,
		NATSPublish:     noopPublish,
		SubjectPrefix:   "p",
		InboundCh:       make(chan struct{}),
		IdleTTL:         time.Minute,
		Clock:           clock.RealClock{},
		ResolveErr:      resolveErr,
		Artifacts:       fakeArtifactService(),
	}
}

func TestToolSetEquivalenceAndDelta(t *testing.T) {
	type scenario struct {
		name string
		// setup runs at the start of each of the two subtests (grant-all and
		// grant-none), before deps() is invoked, to put process-global state
		// (the asset-renderer registry) into the shape this row needs. nil
		// when a row needs no global setup.
		setup  func(t *testing.T)
		legacy []string
		deps   func(caps map[string]apiextensionsv1.JSON) AssembleDeps
	}

	scenarios := []scenario{
		{
			name: "kubectl",
			legacy: []string{
				"agent_work_complete", "introspect_tool", "new_operation",
				"query_knowledge", "query_memory", "record_observation", "search_memory", "update_plan",
			},
			deps: func(caps map[string]apiextensionsv1.JSON) AssembleDeps {
				return AssembleDeps{
					Class:  &spiceboxv1alpha1.AgentClass{Spec: spiceboxv1alpha1.AgentClassSpec{Capabilities: caps}},
					Env:    RunnerEnv{MemoryAvailable: true, SearchAvailable: true, KGAvailable: true},
					Logger: logr.Discard(),
				}
			},
		},
		{
			// "channel, routing=all, no renderer". Not mention_only (thread_history
			// and mention_lookup's tool both stay out of scope for other reasons
			// below), and no renderer registered (artifacts skips even when
			// granted). ResolveErr is set so mention_lookup (SkipReason) and
			// channel_history (quiet-inactive) both decline without needing a
			// live resolve fixture — matching the brief's "no thread/channel
			// history" note. thread_history is excluded independently via
			// RoutingMode != "mention_only". lookup_user_for_mention is EXCLUDED
			// from this row's legacy set: the brief's matrix row for "routing=all,
			// no renderer" only calls out "+ respond_to_user, await_user_message,
			// update_status" with no mention of lookup_user_for_mention, so we
			// reproduce that by declining resolve for this row (consistent in
			// both grant-all and grant-none — the delta invariant holds either
			// way since mention_lookup is default-on, not opt-in).
			name: "channel, routing=all, no renderer",
			setup: func(t *testing.T) {
				clearRenderers(t) // empty registry -> AvailableAssetKinds() == nil regardless of caps
			},
			legacy: []string{
				"agent_work_complete", "introspect_tool", "new_operation",
				"query_knowledge", "query_memory", "record_observation", "search_memory", "update_plan",
				"respond_to_user", "await_user_message", "update_status", "set_thread_title",
			},
			deps: func(caps map[string]apiextensionsv1.JSON) AssembleDeps {
				binding, sess := channelBaseFixture("", []string{})
				return AssembleDeps{
					Class:   &spiceboxv1alpha1.AgentClass{Spec: spiceboxv1alpha1.AgentClassSpec{Capabilities: caps}},
					Session: sess,
					Binding: binding,
					Env:     baseChannelEnv(errors.New("no resolve fixture for this row")),
					Logger:  logr.Discard(),
				}
			},
		},
		{
			// "channel, mention_only". Uses a REAL slack.Kind as ResolvedKind (not
			// pkg/channels/channelkinds/fake, which advertises no SupportedMentionLookups —
			// see channelsourced_test.go's TestMentionLookupNilWhenKindUnsupported)
			// so BOTH lookup_user_for_mention (needs SupportedMentionLookups
			// non-empty) and read_channel_history (needs the kind to implement
			// ChannelHistoryReader) activate; slack.Kind{} satisfies both without
			// any network I/O at Offer time (SupportedMentionLookups/
			// ChannelHistoryBounds are static). ResolvedChannel opts the Channel
			// into channelHistory, and the session's OutputChannel is left nil
			// (== input), satisfying channelhistorygate.Offer's no-leakage branch.
			// This row DOES satisfy the "+ read_channel_history iff
			// channelhistorygate opts in" clause.
			name: "channel, mention_only",
			setup: func(t *testing.T) {
				clearRenderers(t) // no renderer in this row either
			},
			legacy: []string{
				"agent_work_complete", "introspect_tool", "new_operation",
				"query_knowledge", "query_memory", "record_observation", "search_memory", "update_plan",
				"respond_to_user", "await_user_message", "update_status", "set_thread_title",
				"read_thread_history", "lookup_user_for_mention", "read_channel_history",
			},
			deps: func(caps map[string]apiextensionsv1.JSON) AssembleDeps {
				binding, sess := channelBaseFixture("mention_only", nil)
				env := baseChannelEnv(nil)
				env.ResolvedChannel = &spiceboxv1alpha1.Channel{
					Spec: spiceboxv1alpha1.ChannelSpec{
						Kind:           "slack",
						ChannelHistory: &spiceboxv1alpha1.ChannelHistorySpec{Enabled: true},
					},
				}
				env.ResolvedKind = &slack.Kind{}
				env.ResolvedSecret = &corev1.Secret{}
				return AssembleDeps{
					Class:   &spiceboxv1alpha1.AgentClass{Spec: spiceboxv1alpha1.AgentClassSpec{Capabilities: caps}},
					Session: sess,
					Binding: binding,
					Env:     env,
					Logger:  logr.Discard(),
				}
			},
		},
		{
			// "channel + renderer". Same base as "routing=all, no renderer" (same
			// legacy channel set, ResolveErr declines mention_lookup/
			// channel_history the same way) plus a channel that advertises
			// asset:image/png and a Standalone renderer registered for it, so
			// artifacts activates when granted.
			name: "channel + renderer",
			setup: func(t *testing.T) {
				registerStandaloneRenderer(t) // fakestandalone, image/png, Standalone
			},
			legacy: []string{
				"agent_work_complete", "introspect_tool", "new_operation",
				"query_knowledge", "query_memory", "record_observation", "search_memory", "update_plan",
				"respond_to_user", "await_user_message", "update_status", "set_thread_title",
				"artifact_prepare", "artifact_await", "artifact_history", "artifact_offer_view",
			},
			deps: func(caps map[string]apiextensionsv1.JSON) AssembleDeps {
				binding, sess := channelBaseFixture("", []string{"asset:image/png"})
				return AssembleDeps{
					Class:   &spiceboxv1alpha1.AgentClass{Spec: spiceboxv1alpha1.AgentClassSpec{Capabilities: caps}},
					Session: sess,
					Binding: binding,
					Env:     baseChannelEnv(errors.New("no resolve fixture for this row")),
					Logger:  logr.Discard(),
				}
			},
		},
		{
			// "channel + skills". Same base as "routing=all, no renderer" (same
			// legacy channel set) plus a resolved skill body, so the default-on
			// skills capability contributes load_skill. load_skill is NOT an
			// opt-in family, so it survives grant-none.
			name: "channel + skills",
			setup: func(t *testing.T) {
				clearRenderers(t)
			},
			legacy: []string{
				"agent_work_complete", "introspect_tool", "new_operation",
				"query_knowledge", "query_memory", "record_observation", "search_memory", "update_plan",
				"respond_to_user", "await_user_message", "update_status", "set_thread_title",
				"load_skill",
			},
			deps: func(caps map[string]apiextensionsv1.JSON) AssembleDeps {
				binding, sess := channelBaseFixture("", []string{})
				env := baseChannelEnv(errors.New("no resolve fixture for this row"))
				env.SkillBodies = map[string]string{"acme/greet@v1": "# Greet\n\nBody."}
				return AssembleDeps{
					Class:   &spiceboxv1alpha1.AgentClass{Spec: spiceboxv1alpha1.AgentClassSpec{Capabilities: caps}},
					Session: sess,
					Binding: binding,
					Env:     env,
					Logger:  logr.Discard(),
				}
			},
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name+": grant-all reproduces legacy set exactly", func(t *testing.T) {
			if sc.setup != nil {
				sc.setup(t)
			}
			assert.Equal(t, sortedCopy(sc.legacy), names(sc.deps(grantAllOptIns())),
				"granting all opt-ins must reproduce the pre-refactor tool set")
		})
		t.Run(sc.name+": grant-none removes only opt-in families", func(t *testing.T) {
			if sc.setup != nil {
				sc.setup(t)
			}
			assert.Equal(t, withoutOptIn(sc.legacy), names(sc.deps(nil)),
				"granting nothing must differ from legacy only by the opt-in families")
		})
	}
}
