package pipeline

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/sessionscope"
)

// triggerSlotFakeState is the mutable, test-controlled answer
// triggerSlotFakeReceiver.TriggerSlotInstances returns. Package-level because
// registration (below) happens once for the whole test binary; every test
// using it resets both fields first via resetTriggerSlotFake.
type triggerSlotFakeState struct {
	instances []channelkinds.TriggerSlotInstance
	err       error
}

var triggerSlotFake = &triggerSlotFakeState{}

// resetTriggerSlotFake clears the fake provider's canned answer before a
// test configures it, and again on cleanup, so no test's answer leaks into
// the next. None of the tests in this file run in parallel with each other,
// so sequential reuse of one package-level fake is safe.
func resetTriggerSlotFake(t *testing.T) {
	t.Helper()
	triggerSlotFake.instances = nil
	triggerSlotFake.err = nil
	t.Cleanup(func() {
		triggerSlotFake.instances = nil
		triggerSlotFake.err = nil
	})
}

// triggerSlotFakeReceiver's Verify/Translate are never exercised by these
// tests — only its TriggerSlotProvider half is.
type triggerSlotFakeReceiver struct{}

func (triggerSlotFakeReceiver) Verify(context.Context, channelkinds.WebhookSecrets, channelkinds.WebhookRequest) error {
	return nil
}

func (triggerSlotFakeReceiver) Translate(context.Context, *spiceboxv1alpha1.Channel, channelkinds.WebhookRequest) (*channelkinds.WebhookInbound, error) {
	return nil, nil
}

func (triggerSlotFakeReceiver) TriggerSlotInstances(*spiceboxv1alpha1.Channel, string, []byte) ([]channelkinds.TriggerSlotInstance, error) {
	return triggerSlotFake.instances, triggerSlotFake.err
}

var (
	_ channelkinds.WebhookReceiver     = triggerSlotFakeReceiver{}
	_ channelkinds.TriggerSlotProvider = triggerSlotFakeReceiver{}
)

// triggerSlotFakeKind is a registered channel kind whose WebhookReceiver
// implements TriggerSlotProvider, so BindTriggerSlots' kind-instance path (no
// per-slot TriggerInstance expression) has something real to discover via
// the type assertion, distinct from github's receiver.
type triggerSlotFakeKind struct{ fakekind.Kind }

func (triggerSlotFakeKind) Name() string { return "trigger-slot-fake" }

func (triggerSlotFakeKind) WebhookReceiver(channelkinds.Deps) channelkinds.WebhookReceiver {
	return triggerSlotFakeReceiver{}
}

func init() { chregistry.Register(triggerSlotFakeKind{}) }

// triggerSlotFakeChannel builds an input Channel of the fake provider kind.
func triggerSlotFakeChannel(name string) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:       "trigger-slot-fake",
			Role:       spiceboxv1alpha1.ChannelRoleInput,
			AgentClass: "ac1",
		},
	}
}

// classWithSlots builds a minimal AgentClass carrying exactly the given
// instance-axis slot declarations — the only field these tests exercise.
// classWithSlots builds a minimal AgentClass carrying exactly the given
// instance-axis slot declarations, plus a Status.ResolvedSlots entry mirroring
// each one 1:1 with no transform chain — the shape an AgentClass actually
// carries once the reconciler has run and found no keying tool for any of
// these types, which is what every test in this file except the two
// unresolved-status ones is really exercising. A test that needs a DIFFERENT
// status — an explicit transform chain, or status left unresolved — sets
// class.Status.ResolvedSlots itself after calling this.
func classWithSlots(slots ...spiceboxv1alpha1.AuthzSlot) *spiceboxv1alpha1.AgentClass {
	resolved := make([]spiceboxv1alpha1.ResolvedSlot, 0, len(slots))
	for _, s := range slots {
		resolved = append(resolved, spiceboxv1alpha1.ResolvedSlot{ResourceType: s.ResourceType, Permission: s.Permission})
	}
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Authz: &spiceboxv1alpha1.AuthzBlock{Slots: slots},
		},
	}
	ac.Status.ResolvedSlots = resolved
	return ac
}

func triggerTestSession() *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "s1"},
	}
}

// capturingLogContext wraps ctx with a logr that appends every Info call's
// joined args to *out, for tests that assert on a specific log line rather
// than merely on returned state.
func capturingLogContext(out *[]string) context.Context {
	var mu sync.Mutex
	l := funcr.New(func(_, args string) {
		mu.Lock()
		defer mu.Unlock()
		*out = append(*out, args)
	}, funcr.Options{})
	return log.IntoContext(context.Background(), l)
}

// --- TriggerSlotRequestsFor -------------------------------------------------

func TestTriggerSlotRequestsFor_NoSlots_ReturnsNil(t *testing.T) {
	class := classWithSlots()
	assert.Nil(t, TriggerSlotRequestsFor(context.Background(), class))
}

// TestTriggerSlotRequestsFor_NilClass_NoPanic (fix-round m6): Task 5
// constructs a minimal Pipeline and its inputs field-by-field, so a nil
// AgentClass must fail loudly (a log, an empty result), never panic.
func TestTriggerSlotRequestsFor_NilClass_NoPanic(t *testing.T) {
	var reqs []TriggerSlotRequest
	assert.NotPanics(t, func() {
		reqs = TriggerSlotRequestsFor(context.Background(), nil)
	})
	assert.Empty(t, reqs)
}

// TestTriggerSlotRequestsFor_NonTriggerFillFrom_ExcludesTheSlot is the
// narrowing test Step 5's mutation must redden: dropping the AllowsFill filter
// makes this slot's request appear even though it never declared trigger.
func TestTriggerSlotRequestsFor_NonTriggerFillFrom_ExcludesTheSlot(t *testing.T) {
	class := classWithSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "github_pull_request", Permission: "write_memory",
		FillFrom: []string{"query"},
	})
	assert.Empty(t, TriggerSlotRequestsFor(context.Background(), class), "a slot excluding trigger from fillFrom must yield no request")
}

func TestTriggerSlotRequestsFor_TriggerFillFrom_NoExpr_YieldsRequestWithNilExpr(t *testing.T) {
	class := classWithSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "github_pull_request", Permission: "write_memory",
		FillFrom: []string{"trigger"},
	})
	reqs := TriggerSlotRequestsFor(context.Background(), class)
	require.Len(t, reqs, 1)
	assert.Equal(t, "github_pull_request", reqs[0].ResourceType)
	assert.Equal(t, "write_memory", reqs[0].Permission)
	assert.Nil(t, reqs[0].Expr, "no triggerInstance declared: relies on the kind's TriggerSlotProvider")
}

func TestTriggerSlotRequestsFor_TriggerInstance_CompilesIntoExpr(t *testing.T) {
	class := classWithSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "github_pull_request", Permission: "write_memory",
		FillFrom: []string{"trigger"}, TriggerInstance: `payload.custom_id`,
	})
	reqs := TriggerSlotRequestsFor(context.Background(), class)
	require.Len(t, reqs, 1)
	assert.NotNil(t, reqs[0].Expr)
}

func TestTriggerSlotRequestsFor_MalformedTriggerInstance_SkipsTheSlot(t *testing.T) {
	class := classWithSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "github_pull_request", Permission: "write_memory",
		FillFrom: []string{"trigger"}, TriggerInstance: `this is not )( valid cel`,
	})
	assert.Empty(t, TriggerSlotRequestsFor(context.Background(), class), "a triggerInstance that fails to compile must skip the slot, not panic")
}

// TestTriggerSlotRequestsFor_UnsetFillFrom_NeverImpliesTrigger is the
// controller ruling (task-4 fix round, M3): unlike every other fill source,
// trigger eligibility requires EXPLICIT naming — an unset fillFrom must
// never make a slot trigger-eligible, however permissive that reads for
// every other source on the same slot.
func TestTriggerSlotRequestsFor_UnsetFillFrom_NeverImpliesTrigger(t *testing.T) {
	class := classWithSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "github_pull_request", Permission: "write_memory",
		// FillFrom deliberately unset.
	})
	assert.Empty(t, TriggerSlotRequestsFor(context.Background(), class),
		"an unset fillFrom must not make a slot trigger-eligible")
}

// TestTriggerSlotRequestsFor_TriggerInstanceWithoutFillFromNamingTrigger_EmitsNoRequest
// is the other half of the same ruling applied to the expression path: a
// slot carrying a TriggerInstance expression is excluded exactly like a
// bare fillFrom-less slot when fillFrom does not name trigger — the
// expression's presence is not itself an opt-in.
func TestTriggerSlotRequestsFor_TriggerInstanceWithoutFillFromNamingTrigger_EmitsNoRequest(t *testing.T) {
	class := classWithSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "github_pull_request", Permission: "write_memory",
		TriggerInstance: `payload.custom_id`,
		// FillFrom deliberately unset — the expression alone must not opt in.
	})
	assert.Empty(t, TriggerSlotRequestsFor(context.Background(), class),
		"a triggerInstance expression does not itself opt a slot into the trigger source")
}

// TestTriggerSlotRequestsFor_UnresolvedStatus_SkipsAndLogs pins the
// fail-closed direction: ResolvedSlots is published once per declared slot by
// the reconciler, so an EMPTY list on a class that declares a trigger-eligible
// slot means the reconciler has not run yet — not that no slot publishes a
// transform chain. The two reasons are otherwise indistinguishable from the
// list alone, so this must skip the request rather than risk binding a
// resourceType whose chain has not been published yet.
func TestTriggerSlotRequestsFor_UnresolvedStatus_SkipsAndLogs(t *testing.T) {
	class := classWithSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "github_pull_request", Permission: "write_memory",
		FillFrom: []string{"trigger"},
	})
	// Override classWithSlots' usual auto-populated status: this test is
	// specifically about the shape of a class whose reconcile has not run yet.
	class.Status.ResolvedSlots = nil

	var logged []string
	ctx := capturingLogContext(&logged)
	reqs := TriggerSlotRequestsFor(ctx, class)
	assert.Empty(t, reqs, "an unresolved status must skip the request, not bind verbatim")

	joined := strings.Join(logged, "\n")
	assert.Contains(t, joined, "ResolvedSlots", "the log must say what is missing")
}

// TestTriggerSlotRequestsFor_ResolvedStatusEmptyTransforms_YieldsRequest is
// the other direction: a class whose status HAS been published, and whose
// published entry simply carries no transform chain, must still yield a
// request — the existing, intended behavior the skip above must not disturb.
func TestTriggerSlotRequestsFor_ResolvedStatusEmptyTransforms_YieldsRequest(t *testing.T) {
	class := classWithSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "github_pull_request", Permission: "write_memory",
		FillFrom: []string{"trigger"},
	})
	class.Status.ResolvedSlots = []spiceboxv1alpha1.ResolvedSlot{
		{ResourceType: "github_pull_request", Permission: "write_memory"},
	}

	reqs := TriggerSlotRequestsFor(context.Background(), class)
	require.Len(t, reqs, 1)
	assert.Equal(t, "github_pull_request", reqs[0].ResourceType)
}

// TestTriggerSlotRequestsFor_TransformPublishedType_EmitsNoRequest is the
// fix-round M5 ruling: the trigger path binds an id VERBATIM (nil
// transforms — see BindTriggerSlots), while every other fill source runs
// the type's declared ValueTransforms chain. A resourceType that publishes
// one is excluded here, with a log naming why, so the trigger path can
// never produce a second, un-transformed spelling of an instance another
// source already binds through the chain.
func TestTriggerSlotRequestsFor_TransformPublishedType_EmitsNoRequest(t *testing.T) {
	class := classWithSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "github_pull_request", Permission: "write_memory",
		FillFrom: []string{"trigger"},
	})
	class.Status.ResolvedSlots = []spiceboxv1alpha1.ResolvedSlot{
		{ResourceType: "github_pull_request", Permission: "write_memory", ValueTransforms: []string{"lowercase"}},
	}

	var logged []string
	ctx := capturingLogContext(&logged)
	reqs := TriggerSlotRequestsFor(ctx, class)
	assert.Empty(t, reqs, "a resourceType publishing a transform chain must not become a trigger request")

	joined := strings.Join(logged, "\n")
	assert.Contains(t, joined, "transform")
}

// --- BindTriggerSlots --------------------------------------------------------

// TestBindTriggerSlots_ProviderDerivedInstance_BindsExactlyOne pins the
// worked example: a slot with no triggerInstance relies on the kind's
// TriggerSlotProvider (github's receiver), and the pull_request payload's
// node_id becomes the bound instance, at the passed expiry.
func TestBindTriggerSlots_ProviderDerivedInstance_BindsExactlyOne(t *testing.T) {
	class := classWithSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "github_pull_request", Permission: "write_memory",
		FillFrom: []string{"trigger"},
	})
	reqs := TriggerSlotRequestsFor(context.Background(), class)
	require.Len(t, reqs, 1)

	az := &fakeAuthz{}
	p := &Pipeline{Authz: az}
	ch := newGitHubChannel("gh-in", "service:demo")
	ev := channelkinds.InboundEvent{
		Channel:       ch,
		DeliveryEvent: "pull_request",
		RawDelivery: []byte(`{"action":"opened","number":7,"repository":{"full_name":"acme/widgets"},` +
			`"pull_request":{"node_id":"PR_kwABCDEF"}}`),
	}
	expiresAt := time.Unix(1_700_000_000, 0).UTC()

	p.BindTriggerSlots(context.Background(), triggerTestSession(), ev, reqs, expiresAt)

	require.Len(t, az.grantedSlots, 1)
	got := az.grantedSlots[0]
	assert.Equal(t, "github_pull_request", got.ResourceType)
	assert.Equal(t, "PR_kwABCDEF", got.ResourceID.String())
	assert.Equal(t, "write_memory", got.Permission)
	assert.Equal(t, "default/s1", az.grantedSlotsSession)
	assert.True(t, expiresAt.Equal(az.grantedSlotsExpiry))
}

// TestBindTriggerSlots_NonTriggerSlot_BindsNothing is the end-to-end half of
// the narrowing property: a request list built from a slot that excludes
// trigger is empty, and BindTriggerSlots never calls GrantSlots for it.
func TestBindTriggerSlots_NonTriggerSlot_BindsNothing(t *testing.T) {
	class := classWithSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "github_pull_request", Permission: "write_memory",
		FillFrom: []string{"query"},
	})
	reqs := TriggerSlotRequestsFor(context.Background(), class)
	require.Empty(t, reqs)

	az := &fakeAuthz{}
	p := &Pipeline{Authz: az}
	ch := newGitHubChannel("gh-in", "service:demo")
	ev := channelkinds.InboundEvent{
		Channel: ch, DeliveryEvent: "pull_request",
		RawDelivery: []byte(`{"action":"opened","number":7,"repository":{"full_name":"acme/widgets"},` +
			`"pull_request":{"node_id":"PR_kwABCDEF"}}`),
	}
	p.BindTriggerSlots(context.Background(), triggerTestSession(), ev, reqs, time.Now())
	assert.Empty(t, az.grantedSlots)
}

// TestTriggerSlots_CarriesOccupancyAndRebind is the trigger family's
// propagation guard: the slot's occupancy/rebind must ride from the AuthzSlot
// onto the request (TriggerSlotRequestsFor) and from the request onto every
// bound SlotBinding (BindTriggerSlots). A drop in either hop would land a
// verified-delivery instance at GrantSlots reading as single — fail-closed, but
// a multi slot's second instance would be refused, with nothing red.
func TestTriggerSlots_CarriesOccupancyAndRebind(t *testing.T) {
	class := classWithSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "github_pull_request", Permission: "write_memory",
		FillFrom: []string{"trigger"}, TriggerInstance: `payload.custom_id`,
		Occupancy: "multi", Rebind: "approval",
	})
	reqs := TriggerSlotRequestsFor(context.Background(), class)
	require.Len(t, reqs, 1)
	assert.Equal(t, "multi", reqs[0].Occupancy, "the slot's occupancy must reach the request")
	assert.Equal(t, "approval", reqs[0].Rebind)

	az := &fakeAuthz{}
	p := &Pipeline{Authz: az}
	ch := newGitHubChannel("gh-in", "service:demo")
	ev := channelkinds.InboundEvent{
		Channel: ch, DeliveryEvent: "pull_request",
		RawDelivery: []byte(`{"action":"opened","number":7,"repository":{"full_name":"acme/widgets"},"custom_id":"pr-123"}`),
	}
	p.BindTriggerSlots(context.Background(), triggerTestSession(), ev, reqs, time.Now())
	require.Len(t, az.grantedSlots, 1)
	assert.Equal(t, "multi", az.grantedSlots[0].Occupancy, "the request's occupancy must reach the bound slot")
	assert.Equal(t, "approval", az.grantedSlots[0].Rebind)
}

// TestBindTriggerSlots_ExpressionWinsOverProvider crafts the expression and
// the provider to yield DIFFERENT ids for the same slot, and asserts the
// expression's id is the one that binds.
func TestBindTriggerSlots_ExpressionWinsOverProvider(t *testing.T) {
	class := classWithSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "github_pull_request", Permission: "write_memory",
		FillFrom: []string{"trigger"}, TriggerInstance: `payload.custom_id`,
	})
	reqs := TriggerSlotRequestsFor(context.Background(), class)
	require.Len(t, reqs, 1)
	require.NotNil(t, reqs[0].Expr)

	az := &fakeAuthz{}
	p := &Pipeline{Authz: az}
	ch := newGitHubChannel("gh-in", "service:demo")
	// The provider (github's receiver) would derive "provider-id" from
	// pull_request.node_id; the expression reads "custom_id" instead.
	ev := channelkinds.InboundEvent{
		Channel: ch, DeliveryEvent: "pull_request",
		RawDelivery: []byte(`{"action":"opened","number":7,"repository":{"full_name":"acme/widgets"},` +
			`"custom_id":"expr-wins-id","pull_request":{"node_id":"provider-id"}}`),
	}
	p.BindTriggerSlots(context.Background(), triggerTestSession(), ev, reqs, time.Now())

	require.Len(t, az.grantedSlots, 1)
	assert.Equal(t, "github_pull_request", az.grantedSlots[0].ResourceType)
	assert.Equal(t, "expr-wins-id", az.grantedSlots[0].ResourceID.String())
}

// TestBindTriggerSlots_KindInstance_MatchesOnlyItsOwnResourceType (fix-round
// M2) pins the resourceType-match filter on the provider path: a kind
// offering instances of TWO different types in one delivery must bind each
// only into the slot naming ITS type, with ITS permission — never let one
// slot's request absorb another's instances.
func TestBindTriggerSlots_KindInstance_MatchesOnlyItsOwnResourceType(t *testing.T) {
	resetTriggerSlotFake(t)
	triggerSlotFake.instances = []channelkinds.TriggerSlotInstance{
		{ResourceType: "widget", ResourceID: "w1"},
		{ResourceType: "gadget", ResourceID: "g1"},
	}
	class := classWithSlots(
		spiceboxv1alpha1.AuthzSlot{ResourceType: "widget", Permission: "write_memory", FillFrom: []string{"trigger"}},
		spiceboxv1alpha1.AuthzSlot{ResourceType: "gadget", Permission: "read_memory", FillFrom: []string{"trigger"}},
	)
	reqs := TriggerSlotRequestsFor(context.Background(), class)
	require.Len(t, reqs, 2)

	az := &fakeAuthz{}
	p := &Pipeline{Authz: az}
	ev := channelkinds.InboundEvent{Channel: triggerSlotFakeChannel("fake-in"), DeliveryEvent: "widget_event", RawDelivery: []byte(`{}`)}
	p.BindTriggerSlots(context.Background(), triggerTestSession(), ev, reqs, time.Now())

	require.Len(t, az.grantedSlots, 2, "each type's own instance must bind, and only that one")
	byType := map[string]authz.SlotBinding{}
	for _, b := range az.grantedSlots {
		byType[b.ResourceType] = b
	}
	require.Contains(t, byType, "widget")
	require.Contains(t, byType, "gadget")
	assert.Equal(t, "w1", byType["widget"].ResourceID.String())
	assert.Equal(t, "write_memory", byType["widget"].Permission)
	assert.Equal(t, "g1", byType["gadget"].ResourceID.String())
	assert.Equal(t, "read_memory", byType["gadget"].Permission)
}

// TestBindTriggerSlots_NoTypeMatch_LogsMismatch pins the silent-none case: the
// kind DID report instances, but none of them match the requesting slot's
// resourceType. Nothing binds — and unlike every other refusal path in
// BindTriggerSlots, this one must still log, naming the mismatch, so an
// author sees why a delivery that plainly carried instances left their slot
// empty.
func TestBindTriggerSlots_NoTypeMatch_LogsMismatch(t *testing.T) {
	resetTriggerSlotFake(t)
	triggerSlotFake.instances = []channelkinds.TriggerSlotInstance{
		{ResourceType: "gadget", ResourceID: "g1"},
	}
	class := classWithSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "widget", Permission: "write_memory", FillFrom: []string{"trigger"},
	})
	reqs := TriggerSlotRequestsFor(context.Background(), class)
	require.Len(t, reqs, 1)

	az := &fakeAuthz{}
	p := &Pipeline{Authz: az}
	ev := channelkinds.InboundEvent{Channel: triggerSlotFakeChannel("fake-in"), DeliveryEvent: "gadget_event", RawDelivery: []byte(`{}`)}

	var logged []string
	ctx := capturingLogContext(&logged)
	p.BindTriggerSlots(ctx, triggerTestSession(), ev, reqs, time.Now())

	assert.Empty(t, az.grantedSlots, "no instance of the requested type: nothing binds")

	joined := strings.Join(logged, "\n")
	assert.Contains(t, joined, "widget", "the log must name the resourceType the slot requested")
	assert.Contains(t, joined, "gadget", "the log must name a type the kind actually offered")
}

// TestBindTriggerSlots_KindOffersNothing_NoLog is the negative control for
// the mismatch log above: when the kind reports ZERO instances at all (the
// ordinary case for a delivery event the slot's kind was never meant to
// react to), the mismatch log must NOT fire — logging on every uninteresting
// delivery would be per-delivery noise, not a diagnostic.
func TestBindTriggerSlots_KindOffersNothing_NoLog(t *testing.T) {
	resetTriggerSlotFake(t)
	class := classWithSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "widget", Permission: "write_memory", FillFrom: []string{"trigger"},
	})
	reqs := TriggerSlotRequestsFor(context.Background(), class)
	require.Len(t, reqs, 1)

	az := &fakeAuthz{}
	p := &Pipeline{Authz: az}
	ev := channelkinds.InboundEvent{Channel: triggerSlotFakeChannel("fake-in"), DeliveryEvent: "unrelated_event", RawDelivery: []byte(`{}`)}

	var logged []string
	ctx := capturingLogContext(&logged)
	p.BindTriggerSlots(ctx, triggerTestSession(), ev, reqs, time.Now())

	assert.Empty(t, az.grantedSlots)
	joined := strings.Join(logged, "\n")
	assert.NotContains(t, joined, "widget", "the kind offered nothing at all; this is not the mismatch case and must not log")
}

// TestBindTriggerSlots_DuplicateInstance_BindsOnce (fix-round m8) pins dedup:
// the SAME (type, id, permission) offered twice by the provider must bind
// exactly once, and must not itself count as a dropped-by-cap instance.
func TestBindTriggerSlots_DuplicateInstance_BindsOnce(t *testing.T) {
	resetTriggerSlotFake(t)
	triggerSlotFake.instances = []channelkinds.TriggerSlotInstance{
		{ResourceType: "widget", ResourceID: "w1"},
		{ResourceType: "widget", ResourceID: "w1"},
	}
	class := classWithSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "widget", Permission: "write_memory", FillFrom: []string{"trigger"},
	})
	reqs := TriggerSlotRequestsFor(context.Background(), class)
	require.Len(t, reqs, 1)

	az := &fakeAuthz{}
	p := &Pipeline{Authz: az}
	ev := channelkinds.InboundEvent{Channel: triggerSlotFakeChannel("fake-in"), DeliveryEvent: "widget_event", RawDelivery: []byte(`{}`)}

	var logged []string
	ctx := capturingLogContext(&logged)
	p.BindTriggerSlots(ctx, triggerTestSession(), ev, reqs, time.Now())

	require.Len(t, az.grantedSlots, 1, "the duplicate must not become a second binding")
	joined := strings.Join(logged, "\n")
	assert.NotContains(t, joined, "capped", "a duplicate is not a cap drop and must not be reported as one")
}

// TestBindTriggerSlots_ProviderErrors_ExpressionBackedSlotStillBinds
// (fix-round M4) exercises the derivation-error branch: when the kind's
// TriggerSlotProvider errors, a no-expression slot (which depends entirely
// on the provider) binds nothing, while an expression-backed slot in the
// SAME call is unaffected and still binds.
func TestBindTriggerSlots_ProviderErrors_ExpressionBackedSlotStillBinds(t *testing.T) {
	resetTriggerSlotFake(t)
	triggerSlotFake.err = errors.New("provider exploded")

	class := classWithSlots(
		spiceboxv1alpha1.AuthzSlot{ResourceType: "widget", Permission: "write_memory", FillFrom: []string{"trigger"}},
		spiceboxv1alpha1.AuthzSlot{
			ResourceType: "gadget", Permission: "read_memory",
			FillFrom: []string{"trigger"}, TriggerInstance: `payload.custom_id`,
		},
	)
	reqs := TriggerSlotRequestsFor(context.Background(), class)
	require.Len(t, reqs, 2)

	az := &fakeAuthz{}
	p := &Pipeline{Authz: az}
	ev := channelkinds.InboundEvent{
		Channel: triggerSlotFakeChannel("fake-in"), DeliveryEvent: "widget_event",
		RawDelivery: []byte(`{"custom_id":"gadget-id"}`),
	}
	p.BindTriggerSlots(context.Background(), triggerTestSession(), ev, reqs, time.Now())

	require.Len(t, az.grantedSlots, 1, "only the expression-backed slot may bind when the provider errors")
	assert.Equal(t, "gadget", az.grantedSlots[0].ResourceType)
	assert.Equal(t, "gadget-id", az.grantedSlots[0].ResourceID.String())
}

// TestBindTriggerSlots_RefusalPaths_ZeroBindingsNoGrantCall covers the three
// distinct ways a candidate can fail to become a binding, each verified to
// leave GrantSlots uncalled rather than merely leaving one binding unbound.
func TestBindTriggerSlots_RefusalPaths_ZeroBindingsNoGrantCall(t *testing.T) {
	cases := []struct {
		name    string
		class   *spiceboxv1alpha1.AgentClass
		channel *spiceboxv1alpha1.Channel
		body    []byte
		setup   func(t *testing.T)
	}{
		{
			name: "expression eval error: reference to a key the payload does not have",
			class: classWithSlots(spiceboxv1alpha1.AuthzSlot{
				ResourceType: "github_pull_request", Permission: "write_memory",
				FillFrom: []string{"trigger"}, TriggerInstance: `payload.does_not_exist`,
			}),
			channel: newGitHubChannel("gh-in", "service:demo"),
			body: []byte(`{"action":"opened","number":7,"repository":{"full_name":"acme/widgets"},` +
				`"pull_request":{"node_id":"PR_x"}}`),
		},
		{
			name: "expression yields an empty string",
			class: classWithSlots(spiceboxv1alpha1.AuthzSlot{
				ResourceType: "github_pull_request", Permission: "write_memory",
				FillFrom: []string{"trigger"}, TriggerInstance: `payload.custom_id`,
			}),
			channel: newGitHubChannel("gh-in", "service:demo"),
			body: []byte(`{"action":"opened","number":7,"repository":{"full_name":"acme/widgets"},` +
				`"custom_id":"","pull_request":{"node_id":"PR_x"}}`),
		},
		{
			name: "provider-derived id refused by NewObjectID (empty)",
			class: classWithSlots(spiceboxv1alpha1.AuthzSlot{
				ResourceType: "widget", Permission: "write_memory",
				FillFrom: []string{"trigger"},
			}),
			channel: triggerSlotFakeChannel("fake-in"),
			body:    []byte(`{}`),
			setup: func(t *testing.T) {
				resetTriggerSlotFake(t)
				triggerSlotFake.instances = []channelkinds.TriggerSlotInstance{{ResourceType: "widget", ResourceID: ""}}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setup != nil {
				tc.setup(t)
			}
			reqs := TriggerSlotRequestsFor(context.Background(), tc.class)
			require.Len(t, reqs, 1)

			az := &fakeAuthz{}
			p := &Pipeline{Authz: az}
			ev := channelkinds.InboundEvent{Channel: tc.channel, DeliveryEvent: "pull_request", RawDelivery: tc.body}
			p.BindTriggerSlots(context.Background(), triggerTestSession(), ev, reqs, time.Now())

			assert.Empty(t, az.grantedSlots, "a refused candidate must bind nothing")
			assert.Empty(t, az.grantedSlotsSession, "GrantSlots must never be called with zero bindings")
		})
	}
}

// TestBindTriggerSlots_CapsAtMax pins the 8-instance cap: 9 candidates from
// one delivery bind 8 and log the 1 dropped, never silently truncating.
func TestBindTriggerSlots_CapsAtMax(t *testing.T) {
	resetTriggerSlotFake(t)
	instances := make([]channelkinds.TriggerSlotInstance, 0, 9)
	for i := 0; i < 9; i++ {
		instances = append(instances, channelkinds.TriggerSlotInstance{
			ResourceType: "widget", ResourceID: "id-" + string(rune('1'+i)),
		})
	}
	triggerSlotFake.instances = instances

	class := classWithSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "widget", Permission: "write_memory", FillFrom: []string{"trigger"},
	})
	reqs := TriggerSlotRequestsFor(context.Background(), class)
	require.Len(t, reqs, 1)

	az := &fakeAuthz{}
	p := &Pipeline{Authz: az}
	ev := channelkinds.InboundEvent{Channel: triggerSlotFakeChannel("fake-in"), DeliveryEvent: "widget_event", RawDelivery: []byte(`{}`)}

	var logged []string
	ctx := capturingLogContext(&logged)
	p.BindTriggerSlots(ctx, triggerTestSession(), ev, reqs, time.Now())

	assert.Len(t, az.grantedSlots, maxTriggerBoundBindings, "must bind exactly the cap, not the full 9")

	joined := strings.Join(logged, "\n")
	assert.Contains(t, joined, "capped")
	assert.Contains(t, joined, `"dropped"=1`, "exactly one instance must be reported dropped, not silently truncated")
}

// TestBindTriggerSlots_GrantSlotsError_NoPanic pins the fail-closed-but-loud
// behavior: a GrantSlots failure must not panic the caller and must be
// logged, naming the consequence.
func TestBindTriggerSlots_GrantSlotsError_NoPanic(t *testing.T) {
	class := classWithSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "github_pull_request", Permission: "write_memory",
		FillFrom: []string{"trigger"},
	})
	reqs := TriggerSlotRequestsFor(context.Background(), class)
	require.Len(t, reqs, 1)

	az := &fakeAuthz{grantSlotsErr: errors.New("spicedb unavailable")}
	p := &Pipeline{Authz: az}
	ch := newGitHubChannel("gh-in", "service:demo")
	ev := channelkinds.InboundEvent{
		Channel: ch, DeliveryEvent: "pull_request",
		RawDelivery: []byte(`{"action":"opened","number":7,"repository":{"full_name":"acme/widgets"},` +
			`"pull_request":{"node_id":"PR_x"}}`),
	}

	var logged []string
	ctx := capturingLogContext(&logged)
	assert.NotPanics(t, func() {
		p.BindTriggerSlots(ctx, triggerTestSession(), ev, reqs, time.Now())
	})
	assert.Empty(t, az.grantedSlots, "the fake's GrantSlots does not record on error")

	joined := ""
	for _, l := range logged {
		joined += l + "\n"
	}
	assert.Contains(t, joined, "grant write failed")
	assert.Contains(t, joined, "nobody to approve on a triggered session")
}

// TestBindTriggerSlots_NilAuthz_NoPanic (fix-round m6): Task 5 constructs a
// minimal Pipeline field-by-field; a Pipeline{} left with a nil Authz must
// fail loudly (a log, no grant attempted), never panic on the nil call.
func TestBindTriggerSlots_NilAuthz_NoPanic(t *testing.T) {
	class := classWithSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "github_pull_request", Permission: "write_memory",
		FillFrom: []string{"trigger"},
	})
	reqs := TriggerSlotRequestsFor(context.Background(), class)
	require.Len(t, reqs, 1)

	p := &Pipeline{}
	ch := newGitHubChannel("gh-in", "service:demo")
	ev := channelkinds.InboundEvent{
		Channel: ch, DeliveryEvent: "pull_request",
		RawDelivery: []byte(`{"action":"opened","number":7,"repository":{"full_name":"acme/widgets"},` +
			`"pull_request":{"node_id":"PR_x"}}`),
	}

	var logged []string
	ctx := capturingLogContext(&logged)
	assert.NotPanics(t, func() {
		p.BindTriggerSlots(ctx, triggerTestSession(), ev, reqs, time.Now())
	})
	assert.Contains(t, strings.Join(logged, "\n"), "nil Authz")
}

// TestBindTriggerSlots_NilChannel_NoPanic (fix-round m6): an InboundEvent
// with no Channel must fail loudly rather than nil-deref on
// ev.Channel.Spec.Kind.
func TestBindTriggerSlots_NilChannel_NoPanic(t *testing.T) {
	class := classWithSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: "github_pull_request", Permission: "write_memory",
		FillFrom: []string{"trigger"},
	})
	reqs := TriggerSlotRequestsFor(context.Background(), class)
	require.Len(t, reqs, 1)

	az := &fakeAuthz{}
	p := &Pipeline{Authz: az}
	ev := channelkinds.InboundEvent{
		Channel: nil, DeliveryEvent: "pull_request",
		RawDelivery: []byte(`{"pull_request":{"node_id":"PR_x"}}`),
	}

	var logged []string
	ctx := capturingLogContext(&logged)
	assert.NotPanics(t, func() {
		p.BindTriggerSlots(ctx, triggerTestSession(), ev, reqs, time.Now())
	})
	assert.Empty(t, az.grantedSlots)
	assert.Contains(t, strings.Join(logged, "\n"), "no Channel")
}

// --- Step 4: the scope-pairing verification ---------------------------------

// TestTriggerBoundInstance_UnnarrowedByScope_DENIED_WhenDefaultsShareItsType
// is the scope-pairing check the task brief calls for: a class declares BOTH
// a class-pinned default and a trigger-fillFrom slot on the SAME resource
// type. BindClassDefaults narrows that type's ScopeResource entry to the
// default's id (X); BindTriggerSlots binds a DIFFERENT instance (Y) the
// verified delivery names, through a SpiceDB slot grant only — the same
// shape seedThreadSlots already uses, with no ScopeResource write of its own.
//
// PINS A FINDING, not a regression: because CheckScopeWithRefs narrows PER
// TYPE once any entry exists for it, Y is DENIED by Layer 2 even though a
// grant authorizes it. This is not new to trigger binding — seedThreadSlots
// has the identical shape and would be denied the same way — and it has NO
// production effect today because CheckScopeWithRefs is not wired into any
// dispatch path (see its own NOT WIRED doc comment). Reported, not fixed
// here: extending either binder to also write a ScopeResource entry is a
// shared decision for whoever wires this axis, not a call to make silently
// inside one of the two binders it would need to change.
func TestTriggerBoundInstance_UnnarrowedByScope_DENIED_WhenDefaultsShareItsType(t *testing.T) {
	const resType = "github_pull_request"
	const perm = "write_memory"
	ctx := memory.WithSystemApproval(context.Background(), "test")

	// The default-bearing slot and the trigger-bearing slot are declared
	// separately (two AuthzSlot entries sharing a resourceType): a single
	// slot cannot carry both, because fillFrom:[trigger] alone would exclude
	// FillDefault and BindClassDefaults would bind nothing for it.
	mem := memory.NewLocal(inmem.NewBackend())
	memScope := memory.Scope{Kind: "session", ID: "default/s1"}
	entities := []authz.BoundEntitySpec{{ResourceType: resType, Permission: perm, Defaults: []string{"X"}}}
	allow := authz.CheckerFunc(func(context.Context, authz.Permission, authz.Inputs) authz.Result {
		return authz.Result{Outcome: authz.OutcomeAllowed}
	})
	require.NoError(t, authz.BindClassDefaults(ctx, mem, memScope,
		authz.SessionRef{Namespace: "default", Name: "s1"}, entities, allow, nil, "alice", time.Now, 0))

	class := classWithSlots(spiceboxv1alpha1.AuthzSlot{
		ResourceType: resType, Permission: perm, FillFrom: []string{"trigger"},
	})
	reqs := TriggerSlotRequestsFor(context.Background(), class)
	require.Len(t, reqs, 1)

	az := &fakeAuthz{}
	p := &Pipeline{Authz: az}
	ch := newGitHubChannel("gh-in", "service:demo")
	ev := channelkinds.InboundEvent{
		Channel: ch, DeliveryEvent: "pull_request",
		RawDelivery: []byte(`{"action":"opened","number":7,"repository":{"full_name":"acme/widgets"},` +
			`"pull_request":{"node_id":"Y"}}`),
	}
	p.BindTriggerSlots(context.Background(), triggerTestSession(), ev, reqs, time.Now().Add(time.Hour))
	require.Len(t, az.grantedSlots, 1, "the trigger binding itself must still succeed")
	triggerID := az.grantedSlots[0].ResourceID.String()
	require.Equal(t, "Y", triggerID)

	sc, ok, err := sessionscope.Get(ctx, mem, memScope)
	require.NoError(t, err)
	require.True(t, ok, "BindClassDefaults must have written the session_scope entry")

	ref := scope.ResourceRef{ResourceType: resType, ID: triggerID}
	res := scope.CheckScopeWithRefs(sc, "", nil, []scope.ResourceRef{ref}, nil)
	assert.False(t, res.OK, "DENIED: the trigger-bound instance is not in scope.Resources, and the type is narrowed by the default's entry")
	assert.Equal(t, scope.ReasonResourceNotInScope, res.Reason)
}

// --- Step 3: the call site, end to end via Deliver --------------------------

// TestDeliverNewSession_BindsTriggerSlot proves the pipeline.go call site
// actually wires TriggerSlotRequestsFor + BindTriggerSlots into Deliver's
// create path: an AgentClass declaring a trigger-fillFrom slot, delivered a
// verified pull_request webhook, ends with the PR's node id granted into
// that slot on the session Deliver just created.
func TestDeliverNewSession_BindsTriggerSlot(t *testing.T) {
	const subj = "service:demo-reviewer"
	ch := newGitHubChannel("gh-in", subj)
	ac := agentClass(t, metav1.ConditionTrue, spiceboxv1alpha1.ReasonAllReferencesResolve, "")
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{
		Slots: []spiceboxv1alpha1.AuthzSlot{
			{ResourceType: "github_pull_request", Permission: "write_memory", FillFrom: []string{"trigger"}},
		},
	}
	// The reconciler publishes one ResolvedSlots entry per declared slot; this
	// test's class needs its own status stamped the same way classWithSlots
	// does, since agentClass (a different, unrelated-purpose helper) does not.
	ac.Status.ResolvedSlots = []spiceboxv1alpha1.ResolvedSlot{
		{ResourceType: "github_pull_request", Permission: "write_memory"},
	}
	p, az, _, _, _ := newPipeline(t, ch, newSlackOutputChannel("gh-out", "C_OUT"), ac)

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:      ch,
		ChannelKey:   "pr:acme/widgets#9",
		MessageText:  "PR #9 opened",
		AuthzSubject: subj,
		RawDelivery: []byte(`{"action":"opened","number":9,"repository":{"full_name":"acme/widgets"},` +
			`"pull_request":{"node_id":"PR_call_site"}}`),
		DeliveryEvent: "pull_request",
	})
	require.NoError(t, err, "Deliver")
	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)

	require.Len(t, az.grantedSlots, 1, "the call site must invoke BindTriggerSlots on the create path")
	got := az.grantedSlots[0]
	assert.Equal(t, "github_pull_request", got.ResourceType)
	assert.Equal(t, "PR_call_site", got.ResourceID.String())
	assert.Equal(t, "write_memory", got.Permission)
	assert.Equal(t, dec.Session.Namespace+"/"+dec.Session.Name, az.grantedSlotsSession)
}

// TestDeliverNewSession_NoTriggerSlots_NoGrantSlotsCall is the negative
// control: a class with no trigger-fillFrom slot at all must not reach
// BindTriggerSlots — TriggerSlotRequestsFor(&class) is empty and the `if
// len(reqs) > 0` guard at the call site short-circuits.
func TestDeliverNewSession_NoTriggerSlots_NoGrantSlotsCall(t *testing.T) {
	const subj = "service:demo-reviewer"
	ch := newGitHubChannel("gh-in", subj)
	p, az, _, _, _ := newPipeline(t, ch, newSlackOutputChannel("gh-out", "C_OUT"))

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:      ch,
		ChannelKey:   "pr:acme/widgets#10",
		MessageText:  "PR #10 opened",
		AuthzSubject: subj,
		RawDelivery: []byte(`{"action":"opened","number":10,"repository":{"full_name":"acme/widgets"},` +
			`"pull_request":{"node_id":"PR_no_slots"}}`),
		DeliveryEvent: "pull_request",
	})
	require.NoError(t, err, "Deliver")
	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)

	assert.Empty(t, az.grantedSlots, "a class with no trigger slot must never call GrantSlots")
}
