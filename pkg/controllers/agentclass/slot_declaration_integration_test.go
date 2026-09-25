//go:build integration

package agentclass_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// slotClassFor builds a minimal-but-valid AgentClass carrying the given slots.
func slotClassFor(name string, slots ...spiceboxv1alpha1.AuthzSlot) *spiceboxv1alpha1.AgentClass {
	ac := newClass(name)
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{Slots: slots}
	return ac
}

// ensureLLMCreds creates the Secret newClass points its model at. Without it the
// class stalls at Valid=False/SecretMissing, which would mask the slot verdict
// this file is actually about.
func ensureLLMCreds(t *testing.T, env *testenv.Env) {
	t.Helper()
	err := env.Client.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
		Data:       map[string][]byte{"api-key": []byte("sk")},
	})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err, "create llm-creds")
	}
}

// The enum markers must be enforced by the APISERVER, not merely present on the
// Go type. A unit test over the validation function cannot tell the difference:
// it passes identically whether the CRD carries the enum or not.
//
// This is the level that catches a marker that never made it through
// `mage gen:api && mage manifests` into the shipped CRD.
func TestSlotDeclaration_CRDEnforcesEnums(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	cases := []struct {
		name      string
		slot      spiceboxv1alpha1.AuthzSlot
		wantAdmit bool
	}{
		{
			name: "valid enum values: admitted",
			slot: spiceboxv1alpha1.AuthzSlot{
				ResourceType: "http_target", Description: "a URL", Permission: "reachable",
				FillFrom: []string{"channel_thread", "ask"}, AutoGrantFrom: []string{"owner"},
				Membership: "frozen",
			},
			wantAdmit: true,
		},
		{
			name: "bogus fillFrom: rejected by the apiserver",
			slot: spiceboxv1alpha1.AuthzSlot{
				ResourceType: "http_target", Description: "a URL", Permission: "reachable",
				FillFrom: []string{"telepathy"},
			},
		},
		{
			name: "bogus autoGrantFrom: rejected by the apiserver",
			slot: spiceboxv1alpha1.AuthzSlot{
				ResourceType: "http_target", Description: "a URL", Permission: "reachable",
				FillFrom: []string{"channel_thread"}, AutoGrantFrom: []string{"everyone"},
			},
		},
		{
			name: "bogus membership: rejected by the apiserver",
			slot: spiceboxv1alpha1.AuthzSlot{
				ResourceType: "http_target", Description: "a URL", Permission: "reachable",
				Membership: "occasional",
			},
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ac := slotClassFor(uniqueName("slot-enum", i), tc.slot)
			err := env.Client.Create(ctx, ac)
			if tc.wantAdmit {
				require.NoError(t, err, "a declaration using only valid enum values must admit")
				t.Cleanup(func() { _ = env.Client.Delete(ctx, ac) })
				return
			}
			require.Error(t, err, "the CRD enum must reject this at admission, not leave it to the reconciler")
		})
	}
}

// membership defaults to FROZEN. The default lives in a kubebuilder marker, so
// like the enums it is only real once it reaches the CRD — and defaulting is
// applied by the apiserver on write, which no unit test exercises.
//
// frozen rather than dynamic because a grant that follows the session member
// set is only safe while that set is hard to widen, and it is not. Following it
// is an explicit opt-in; a slot author who says nothing gets the pinned set.
func TestSlotDeclaration_CRDDefaultsMembershipToFrozen(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	ac := slotClassFor("slot-default-membership", spiceboxv1alpha1.AuthzSlot{
		ResourceType: "tracker_issue", Description: "an issue", Permission: "write",
	})
	require.NoError(t, env.Client.Create(ctx, ac))
	t.Cleanup(func() { _ = env.Client.Delete(ctx, ac) })

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "slot-default-membership"}, &got))
	require.Len(t, got.Spec.GetSlots(), 1)
	assert.Equal(t, "frozen", got.Spec.GetSlots()[0].Membership,
		"an unset membership must default to frozen at admission")
}

// The cross-field rule cannot be expressed as a CRD enum, so it is the
// reconciler's to enforce — and it must land as a Valid=False condition, which
// is what stops a session starting on the class.
//
// This fixture references NO MCPServer and NO toolkit, deliberately. Slot
// validation used to live inside validatePermissions, which only runs when one
// of those is present, so a class declaring slots and no tools was never
// checked. The unit test over the validator passed the whole time — it calls the
// function directly, which is precisely what the reconciler was not doing.
func TestSlotDeclaration_ReconcilerRejectsInertTrustPolicy(t *testing.T) {
	env := testenv.Shared(t)
	ensureLLMCreds(t, env)
	startManager(t, env)
	ctx := context.Background()

	ac := slotClassFor("slot-inert-policy", spiceboxv1alpha1.AuthzSlot{
		ResourceType: "http_target", Description: "a URL", Permission: "reachable",
		// autoGrantFrom with no channel_thread source: the policy can never run.
		FillFrom: []string{"ask"}, AutoGrantFrom: []string{"owner"},
	})
	require.NoError(t, env.Client.Create(ctx, ac))
	t.Cleanup(func() { _ = env.Client.Delete(ctx, ac) })

	eventuallyValid(t, env.Client, "slot-inert-policy",
		metav1.ConditionFalse, spiceboxv1alpha1.ReasonSlotDeclarationInvalid, 30*time.Second)
}

// The companion to the rejection: a well-formed declaration must NOT block the
// class. A validator that rejects everything would pass the test above.
func TestSlotDeclaration_ReconcilerAdmitsAWellFormedSlot(t *testing.T) {
	env := testenv.Shared(t)
	ensureLLMCreds(t, env)
	startManager(t, env)
	ctx := context.Background()

	ac := slotClassFor("slot-well-formed", spiceboxv1alpha1.AuthzSlot{
		ResourceType: "http_target", Description: "a URL", Permission: "reachable",
		FillFrom: []string{"channel_thread", "ask"}, AutoGrantFrom: []string{"owner"},
		Membership: "frozen",
	})
	declareTypesFor(t, env, ac, "http_target")
	require.NoError(t, env.Client.Create(ctx, ac))
	t.Cleanup(func() { _ = env.Client.Delete(ctx, ac) })

	eventuallyValid(t, env.Client, "slot-well-formed", metav1.ConditionTrue, "", 30*time.Second)
}

// The renamed-boundEntities rejection was hidden by the same wiring gap, and it
// is the one that matters most: its whole purpose is to refuse a class whose
// instance constraints would otherwise be silently dropped on upgrade. A class
// with no tools is exactly the shape most likely to be a small hand-written one.
func TestSlotDeclaration_ReconcilerRejectsRenamedBoundEntitiesWithoutTools(t *testing.T) {
	env := testenv.Shared(t)
	ensureLLMCreds(t, env)
	startManager(t, env)
	ctx := context.Background()

	ac := newClass("slot-legacy-no-tools")
	ac.Spec.BoundEntities = []spiceboxv1alpha1.AuthzSlot{
		{ResourceType: "tracker_issue", Description: "an issue", Permission: "write"},
	}
	require.NoError(t, env.Client.Create(ctx, ac))
	t.Cleanup(func() { _ = env.Client.Delete(ctx, ac) })

	eventuallyValid(t, env.Client, "slot-legacy-no-tools",
		metav1.ConditionFalse, spiceboxv1alpha1.ReasonBoundEntitiesRenamed, 30*time.Second)
}

func uniqueName(prefix string, i int) string {
	return prefix + "-" + string(rune('a'+i))
}

// A slot declared on the class must reach the owned AgentSessionGrants CR, which
// is what the guardian controller reads to compose the SpiceDB schema.
//
// This is the wiring the unit tests cannot see. extractSlotPairs is a pure
// function that passes its own tests whether or not anything calls it — the same
// shape of gap that left slot VALIDATION unreachable until an integration test
// caught it.
func TestSlotDeclaration_ReachesTheAgentSessionGrantsCR(t *testing.T) {
	env := testenv.Shared(t)
	ensureLLMCreds(t, env)
	startManager(t, env)
	ctx := context.Background()

	ac := slotClassFor("slot-grants-cr", spiceboxv1alpha1.AuthzSlot{
		ResourceType: "crm_company", Description: "a company", Permission: "contact_access",
	})
	declareTypesFor(t, env, ac, "crm_company")
	require.NoError(t, env.Client.Create(ctx, ac))
	t.Cleanup(func() { _ = env.Client.Delete(ctx, ac) })

	eventuallyValid(t, env.Client, "slot-grants-cr", metav1.ConditionTrue, "", 30*time.Second)

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var asg spiceboxv1alpha1.AgentSessionGrants
		if err := env.Client.Get(ctx, types.NamespacedName{
			Namespace: "default", Name: "slot-grants-cr-grants",
		}, &asg); err == nil {
			if len(asg.Spec.Slots) == 1 &&
				asg.Spec.Slots[0].ResourceType == "crm_company" &&
				asg.Spec.Slots[0].Permission == "contact_access" {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the class's authz.slots never reached AgentSessionGrants/slot-grants-cr-grants spec.slots")
}

// TestSlotDeclaration_ReconcilerRejectsDefaultsThatCanNeverBind covers the
// inert-declaration rule pointing the CAPABILITY direction rather than the
// constraint direction: a class pins an ID and then declares a fillFrom that
// excludes the source those IDs bind through, so the agent starts with nothing
// while the YAML reads as though a repo were already in hand.
//
// Reconciler-level rather than unit-level for the reason the sibling tests
// document: the validator is called directly by its unit test whether or not
// anything invokes it, and this fixture has no MCPServer and no toolkit — the
// exact shape whose slot validation was unreachable until an integration test
// caught it.
func TestSlotDeclaration_ReconcilerRejectsDefaultsThatCanNeverBind(t *testing.T) {
	env := testenv.Shared(t)
	ensureLLMCreds(t, env)
	startManager(t, env)
	ctx := context.Background()

	ac := slotClassFor("slot-inert-defaults", spiceboxv1alpha1.AuthzSlot{
		ResourceType: "github_repo", Description: "a repo", Permission: "read",
		Defaults: []string{"demo-org/demo-repo"}, FillFrom: []string{"ask"},
	})
	require.NoError(t, env.Client.Create(ctx, ac))
	t.Cleanup(func() { _ = env.Client.Delete(ctx, ac) })

	eventuallyValid(t, env.Client, "slot-inert-defaults",
		metav1.ConditionFalse, spiceboxv1alpha1.ReasonSlotDeclarationInvalid, 30*time.Second)
}

// The companion admission: naming the default source makes the same class
// legal, so the rule is not just "any class with defaults is refused".
func TestSlotDeclaration_ReconcilerAdmitsDefaultsWithTheDefaultSource(t *testing.T) {
	env := testenv.Shared(t)
	ensureLLMCreds(t, env)
	startManager(t, env)
	ctx := context.Background()

	ac := slotClassFor("slot-defaults-ok", spiceboxv1alpha1.AuthzSlot{
		ResourceType: "github_repo", Description: "a repo", Permission: "read",
		Defaults: []string{"demo-org/demo-repo"}, FillFrom: []string{"default", "ask"},
	})
	declareTypesFor(t, env, ac, "github_repo")
	require.NoError(t, env.Client.Create(ctx, ac))
	t.Cleanup(func() { _ = env.Client.Delete(ctx, ac) })

	eventuallyValid(t, env.Client, "slot-defaults-ok", metav1.ConditionTrue, "", 30*time.Second)
}

// declareTypesFor creates an MCPServer whose schema fragment classifies each
// named resource type as session-only, and points the class at it.
//
// Needed because standing has no default: a class with a slot on a type no
// fragment declares is refused, so a test whose subject is slot ADMISSION must
// first give the type a classification. session-only is the truthful answer for
// these fixtures — they seed no tuples for anyone to hold.
func declareTypesFor(t *testing.T, env *testenv.Env, ac *spiceboxv1alpha1.AgentClass, types ...string) {
	t.Helper()
	resources := make([]spiceboxv1alpha1.SpiceDBResource, 0, len(types))
	for _, rt := range types {
		resources = append(resources, spiceboxv1alpha1.SpiceDBResource{
			Name: rt, Standing: spiceboxv1alpha1.StandingSessionOnly,
		})
	}
	name := ac.Name + "-types"
	srv := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name:          name,
			Version:       "v1",
			Intent:        "declares resource types for a slot-admission fixture",
			Server:        spiceboxv1alpha1.MCPServerServer{URL: "http://127.0.0.1:1/mcp", Transport: "http"},
			Tools:         []spiceboxv1alpha1.MCPServerTool{},
			SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{Resources: resources},
		},
	}
	err := env.Client.Create(context.Background(), srv)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err, "create declaring MCPServer")
	}
	// The class refuses to resolve against an MCPServer that is not Valid, and
	// nothing in this test env runs the MCPServer controller to make it so.
	srv.Status = spiceboxv1alpha1.MCPServerStatus{Conditions: []metav1.Condition{{
		Type: spiceboxv1alpha1.MCPServerConditionValid, Status: metav1.ConditionTrue,
		Reason: "OK", LastTransitionTime: metav1.Now(),
	}}}
	require.NoError(t, env.Client.Status().Update(context.Background(), srv), "mark declaring MCPServer Valid")
	t.Cleanup(func() { _ = env.Client.Delete(context.Background(), srv) })
	ac.Spec.MCPServers = append(ac.Spec.MCPServers, spiceboxv1alpha1.AgentClassMCPServerRef{Name: name, Ref: name})
}

// wellFormedPrecondition is the shape §4 of the design writes out: a predicate
// over an observed fact, plus the two messages the tri-state needs — one for the
// agent while the fact is unrecorded, one for the human when it refuses.
func wellFormedPrecondition(expr string) spiceboxv1alpha1.SlotPrecondition {
	return spiceboxv1alpha1.SlotPrecondition{
		CEL: expr,
		UndeterminedHint: "before checking this pull request out, establish where its head lives: " +
			"call gitlike_gh with `pr view <n> --json isCrossRepository`",
		RefusalMessage: "this pull request's head branch is on a fork; approving lets the " +
			"reviewer check out and run code from it",
	}
}

// The MinLength=1 markers on a precondition's three message fields must be
// enforced by the APISERVER, not merely present on the Go type — the same
// argument the enum test above makes, and the level that catches a marker which
// never made it through `mage gen:api && mage manifests` into the shipped CRD.
//
// All three are required because they address three different parties: cel is
// the machine's copy of the rule, undeterminedHint is the agent's, and
// refusalMessage is the human's. A precondition missing one of the messages can
// only ever deny in silence.
func TestSlotDeclaration_CRDRequiresPreconditionMessages(t *testing.T) {
	env := testenv.Shared(t)
	ctx := context.Background()

	full := wellFormedPrecondition("facts.observed.is_cross_repository == false")
	noCEL, noHint, noRefusal := full, full, full
	noCEL.CEL = ""
	noHint.UndeterminedHint = ""
	noRefusal.RefusalMessage = ""

	cases := []struct {
		name      string
		pre       spiceboxv1alpha1.SlotPrecondition
		wantAdmit bool
	}{
		{name: "all three fields set: admitted", pre: full, wantAdmit: true},
		{name: "empty cel: rejected by the apiserver", pre: noCEL},
		{name: "empty undeterminedHint: rejected by the apiserver", pre: noHint},
		{name: "empty refusalMessage: rejected by the apiserver", pre: noRefusal},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ac := slotClassFor(uniqueName("slot-precond-crd", i), spiceboxv1alpha1.AuthzSlot{
				ResourceType: "git_commit", Description: "a commit", Permission: "read",
				Requires: []spiceboxv1alpha1.SlotPrecondition{tc.pre},
			})
			err := env.Client.Create(ctx, ac)
			if tc.wantAdmit {
				require.NoError(t, err, "a fully-specified precondition must admit")
				t.Cleanup(func() { _ = env.Client.Delete(ctx, ac) })
				return
			}
			require.Error(t, err, "the CRD MinLength must reject this at admission, not leave it to the reconciler")
		})
	}
}

// A precondition that does not compile is refused at ADMISSION, as a Valid=False
// condition that stops a session starting on the class.
//
// Reconciler-level rather than unit-level for the reason its siblings document:
// the validator is called directly by its unit test whether or not anything
// invokes it. This fixture references no MCPServer and no toolkit — the shape
// whose slot validation was once unreachable.
func TestSlotDeclaration_ReconcilerRejectsAnUncompilablePrecondition(t *testing.T) {
	env := testenv.Shared(t)
	ensureLLMCreds(t, env)
	startManager(t, env)
	ctx := context.Background()

	ac := slotClassFor("slot-precond-bad", spiceboxv1alpha1.AuthzSlot{
		ResourceType: "git_commit", Description: "a commit", Permission: "read",
		// observed so the channel_thread bypass rule (which runs before the
		// predicates are compiled) does not preempt the compile rejection this
		// test is about.
		FillFrom: []string{"observed"},
		// `rumour` is not a provenance. Refused rather than treated as a fact
		// that never arrives, which would hold the slot undetermined forever
		// while its author looked for a missing observation.
		Requires: []spiceboxv1alpha1.SlotPrecondition{
			wellFormedPrecondition("facts.rumour.is_cross_repository == false"),
		},
	})
	// Not decoration. Without a declared standing for git_commit,
	// resolveSlotValueKeying refuses this class under the SAME
	// SlotDeclarationInvalid reason, and eventuallyValid compares status and
	// reason only — so the test would pass with the precondition check deleted
	// entirely. Declaring the type removes that other rejection, and the message
	// assertion below is what names which one actually fired.
	declareTypesFor(t, env, ac, "git_commit")
	require.NoError(t, env.Client.Create(ctx, ac))
	t.Cleanup(func() { _ = env.Client.Delete(ctx, ac) })

	eventuallyValid(t, env.Client, "slot-precond-bad",
		metav1.ConditionFalse, spiceboxv1alpha1.ReasonSlotDeclarationInvalid, 30*time.Second)

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, env.Client.Get(ctx, types.NamespacedName{
		Namespace: "default", Name: "slot-precond-bad",
	}, &got))
	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	require.NotNil(t, cond, "the Valid condition eventuallyValid just matched must still be present")
	assert.Contains(t, cond.Message, "requires[0]",
		"the condition must name the offending entry, not merely carry the shared reason")
	assert.Contains(t, cond.Message, "unknown fact provenance",
		"and it must be the PRECONDITION rejection, not another rule that shares the reason")
}

// The companion admission: a compilable predicate must NOT block the class. A
// validator that refused every precondition would pass the test above.
func TestSlotDeclaration_ReconcilerAdmitsAWellFormedPrecondition(t *testing.T) {
	env := testenv.Shared(t)
	ensureLLMCreds(t, env)
	startManager(t, env)
	ctx := context.Background()

	ac := slotClassFor("slot-precond-ok", spiceboxv1alpha1.AuthzSlot{
		ResourceType: "git_commit", Description: "a commit", Permission: "read",
		// A gated slot must restrict fillFrom to gate-running sources; observed is
		// the intended shape, and an empty fillFrom would be refused for admitting
		// channel_thread.
		FillFrom: []string{"observed"},
		Requires: []spiceboxv1alpha1.SlotPrecondition{
			wellFormedPrecondition("facts.observed.is_cross_repository == false"),
		},
	})
	declareTypesFor(t, env, ac, "git_commit")
	require.NoError(t, env.Client.Create(ctx, ac))
	t.Cleanup(func() { _ = env.Client.Delete(ctx, ac) })

	eventuallyValid(t, env.Client, "slot-precond-ok", metav1.ConditionTrue, "", 30*time.Second)
}

// TestSlotDeclaration_ReconcilerAdmitsAnObservedFillFrom is the row that proves
// the `observed` fill source landed at BOTH mirrors a class declaration passes
// through: env.Client.Create must not error (the CRD's items:Enum marker on
// FillFrom), and eventuallyValid must reach ConditionTrue (the reconciler's
// validFillFrom map in slot_declaration.go). Either mirror missing this value
// fails the test — the CRD would reject the Create, or the reconciler would
// reject the otherwise-valid class as an unknown enum value.
func TestSlotDeclaration_ReconcilerAdmitsAnObservedFillFrom(t *testing.T) {
	env := testenv.Shared(t)
	ensureLLMCreds(t, env)
	startManager(t, env)
	ctx := context.Background()

	ac := slotClassFor("slot-observed-ok", spiceboxv1alpha1.AuthzSlot{
		ResourceType: "git_commit", Description: "a commit", Permission: "read",
		FillFrom: []string{"observed"},
	})
	declareTypesFor(t, env, ac, "git_commit")
	require.NoError(t, env.Client.Create(ctx, ac),
		"a slot declaring only the observed fill source must admit at the apiserver")
	t.Cleanup(func() { _ = env.Client.Delete(ctx, ac) })

	eventuallyValid(t, env.Client, "slot-observed-ok", metav1.ConditionTrue, "", 30*time.Second)
}

// TestSlotDeclaration_ReconcilerAdmitsATriggerFillFrom is
// TestSlotDeclaration_ReconcilerAdmitsAnObservedFillFrom's twin for `trigger`
// — the exact fill source the shipped reviewbot demo's slot names
// (examples/reviewbot/manifests/agentclass.yaml). `trigger` shipped in the
// CRD's kubebuilder items:Enum marker on AuthzSlot.FillFrom without a
// matching entry in the reconciler's own validFillFrom map
// (slot_declaration.go), so a class naming it admitted at the apiserver and
// then reached AgentClass status Valid=False/SlotDeclarationInvalid on every
// reconcile — invisible to `go test ./...`, which never compiles this
// build-tagged file at all. This row is what would have caught it.
//
// A synthetic resourceType (not github_pull_request), same as this file's
// other rows: declareTypesFor's fixture MCPServer would otherwise declare a
// SECOND `github_pull_request` alongside the embedded gh toolkit's own
// fragment, and this test is about the fill-source mirror, not about the
// real gh-toolkit shape.
func TestSlotDeclaration_ReconcilerAdmitsATriggerFillFrom(t *testing.T) {
	env := testenv.Shared(t)
	ensureLLMCreds(t, env)
	startManager(t, env)
	ctx := context.Background()

	ac := slotClassFor("slot-trigger-ok", spiceboxv1alpha1.AuthzSlot{
		ResourceType: "webhook_thing", Description: "a webhook-named instance", Permission: "write_memory",
		FillFrom: []string{"trigger"},
	})
	declareTypesFor(t, env, ac, "webhook_thing")
	require.NoError(t, env.Client.Create(ctx, ac),
		"a slot declaring only the trigger fill source must admit at the apiserver")
	t.Cleanup(func() { _ = env.Client.Delete(ctx, ac) })

	eventuallyValid(t, env.Client, "slot-trigger-ok", metav1.ConditionTrue, "", 30*time.Second)
}
