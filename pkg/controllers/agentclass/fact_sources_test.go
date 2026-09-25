// pkg/controllers/agentclass/fact_sources_test.go
//
// Untagged (unit tier): drives the real Reconcile against a fake client and
// pins the ADMISSION-time half of slot preconditions — the refusal of a
// precondition nothing could ever satisfy, and the status.factSources
// observation that says what a class's facts depend on.
//
// Reconcile rather than the resolver alone on purpose. The resolver is one
// function among a dozen the reconciler could forget to call, and this plan has
// twice shipped a validator that ran on only some class shapes. A test that
// calls resolveFactSources directly proves the rule and says nothing about
// whether any class is ever subject to it.
package agentclass_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// The two resource types every fixture below composes from: the type the slot
// with the precondition is declared on, and an unrelated one a producer can be
// gated by instead.
const (
	gatedType    = "code_review"
	unrelatedTyp = "tracker_issue"
)

// observingTool builds an MCPServer tool that records factName about an
// instance of `about`, gated on gateType. gateType empty leaves the tool
// stateless — a producer nothing gates.
func observingTool(name, gateType, factName, about string) spiceboxv1alpha1.MCPServerTool {
	t := spiceboxv1alpha1.MCPServerTool{
		Name:   name,
		Intent: "records a fact about a review",
	}
	if gateType == "" {
		t.Permission = &authz.Permission{StateImpact: authz.Stateless}
	} else {
		t.Permission = checkOn(gateType)
	}
	if factName != "" {
		t.Observes = []spiceboxv1alpha1.ObservesBlock{{
			Subjects: []spiceboxv1alpha1.ObserveSubject{{
				ResourceType: fmt.Sprintf("%q", about),
				ResourceID:   "string(result.id)",
			}},
			Facts: map[string]string{factName: "result.clean"},
		}}
	}
	return t
}

// declaringServer builds a Valid=True MCPServer carrying the given tools and a
// schema fragment that declares a standing for both resource types. The
// standing is not optional decoration: resolveStandingFor refuses a class whose
// slot type nobody classified, and that refusal would mask every verdict this
// file is about.
func declaringServer(name string, tools ...spiceboxv1alpha1.MCPServerTool) *spiceboxv1alpha1.MCPServer {
	return &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name:    name,
			Version: "v1",
			Intent:  "a fixture server for precondition admission",
			Server:  spiceboxv1alpha1.MCPServerServer{URL: "http://127.0.0.1:1/mcp", Transport: "http"},
			Tools:   tools,
			SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
				Resources: []spiceboxv1alpha1.SpiceDBResource{
					{Name: gatedType, Standing: spiceboxv1alpha1.StandingSessionOnly},
					{Name: unrelatedTyp, Standing: spiceboxv1alpha1.StandingSessionOnly},
				},
			},
		},
		Status: spiceboxv1alpha1.MCPServerStatus{Conditions: []metav1.Condition{{
			Type: spiceboxv1alpha1.MCPServerConditionValid, Status: metav1.ConditionTrue,
			Reason: "OK", LastTransitionTime: metav1.Now(),
		}}},
	}
}

// requiringClass builds a class whose gatedType slot carries one precondition
// over the named observed fact.
func requiringClass(name, factName string) *spiceboxv1alpha1.AgentClass {
	ac := newClass(name)
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{Slots: []spiceboxv1alpha1.AuthzSlot{{
		ResourceType: gatedType,
		Description:  "a review the agent may act on",
		Permission:   "approve",
		// A gated slot must restrict fillFrom to sources that run the precondition
		// gate. An empty fillFrom would admit channel_thread, whose channelsd
		// backfill grants a slot outside the admissibleCandidates filter, so
		// validateSlotDeclarations now refuses requires[] + a channel_thread-admitting
		// fillFrom. observed is the gate-running route these fixtures mean.
		FillFrom: []string{"observed"},
		Requires: []spiceboxv1alpha1.SlotPrecondition{{
			CEL:              fmt.Sprintf("facts.observed.%s == true", factName),
			UndeterminedHint: "call check_review on this review first",
			RefusalMessage:   "this review has not been established as clean",
		}},
	}}}
	return ac
}

func validCondition(t *testing.T, ac *spiceboxv1alpha1.AgentClass) *metav1.Condition {
	t.Helper()
	return findCondition(ac.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
}

// sourceFor returns the published entry for one (provenance, name, producer),
// or nil.
func sourceFor(sources []spiceboxv1alpha1.FactSource, name, producedBy string) *spiceboxv1alpha1.FactSource {
	for i := range sources {
		if sources[i].Name == name && sources[i].ProducedBy == producedBy {
			return &sources[i]
		}
	}
	return nil
}

// A precondition whose ONLY producer is gated by the very slot it holds shut
// can never be satisfied: the slot will not bind until the fact is recorded,
// and the call that records it will not be allowed until the slot binds. The
// agent gets an undeterminedHint naming a call it is about to be denied, and
// burns its budget retrying.
//
// Refused at admission for the same reason the subagent graph is DAG-validated
// there: a bound that only holds at runtime does not hold.
func TestReconcile_RefusesAPreconditionGatedByItsOwnSlot(t *testing.T) {
	ac := requiringClass("factsrc-cycle", "review_is_clean")
	srv := declaringServer("factsrc-cycle-srv",
		observingTool("check_review", gatedType, "review_is_clean", gatedType))
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{
		{Name: srv.Name, Ref: srv.Name},
	}

	got := reconcileClass(t, ac, srv)

	cond := validCondition(t, &got)
	require.NotNil(t, cond, "the class must carry a Valid condition")
	assert.Equal(t, metav1.ConditionFalse, cond.Status,
		"a precondition nothing can satisfy must refuse the class, not hang a session")
	assert.Equal(t, spiceboxv1alpha1.ReasonSlotPreconditionUnsatisfiable, cond.Reason)

	// Both sides named, and the substring is unique to this branch: several
	// other rules in this controller reject with a slot-shaped reason, so a
	// reason-only assertion here would pass with the detection deleted.
	assert.Contains(t, cond.Message, "the very slot this precondition holds shut",
		"the message must say WHY nothing can satisfy the rule")
	assert.Contains(t, cond.Message, "review_is_clean", "name the fact")
	assert.Contains(t, cond.Message, "MCPServer/factsrc-cycle-srv tool/check_review",
		"name the producer that cannot be reached")
	assert.Contains(t, cond.Message, "authz.slots[code_review].requires[0]",
		"address the entry the author wrote")
	// P-10: an approval waives preconditions, so a human COULD break this by
	// hand on every session. An author who meant that must learn why the class
	// was refused rather than guess.
	assert.Contains(t, cond.Message, "without evaluating its preconditions",
		"the refusal must name the waiver that could break the deadlock")

	// The observation is stamped even on the refusal, so an operator reading a
	// refused class can see what its preconditions depend on.
	e := sourceFor(got.Status.FactSources, "review_is_clean",
		"MCPServer/factsrc-cycle-srv tool/check_review")
	require.NotNil(t, e, "factSources must be published on a refused class too: %+v", got.Status.FactSources)
	assert.Equal(t, spiceboxv1alpha1.FactSourceStateGated, e.State)
	assert.Equal(t, []string{gatedType}, e.GatedBy)
}

// The same shape with the producer gated on a DIFFERENT type is a working
// design, not a cycle: the agent binds the other slot, makes the call, the fact
// is recorded, and this slot opens. Admitting it is the whole reason detection
// is a per-slot question rather than "a precondition reads a fact this class
// also produces".
func TestReconcile_AdmitsAPreconditionWhoseProducerIsGatedElsewhere(t *testing.T) {
	ac := requiringClass("factsrc-other-slot", "review_is_clean")
	srv := declaringServer("factsrc-other-slot-srv",
		observingTool("check_review", unrelatedTyp, "review_is_clean", gatedType))
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{
		{Name: srv.Name, Ref: srv.Name},
	}

	got := reconcileClass(t, ac, srv)

	cond := validCondition(t, &got)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"a producer on another slot is reachable; message=%s", cond.Message)

	e := sourceFor(got.Status.FactSources, "review_is_clean",
		"MCPServer/factsrc-other-slot-srv tool/check_review")
	require.NotNil(t, e, "the producer must be published: %+v", got.Status.FactSources)
	assert.Equal(t, spiceboxv1alpha1.FactSourceStateGated, e.State)
	assert.Equal(t, []string{unrelatedTyp}, e.GatedBy,
		"the gate is what distinguishes this class from the refused one")
	assert.Empty(t, e.Detail, "a plain gated producer needs no explanation")
}

// A producer whose BASE check names the slot's own type but which carries a
// permission VARIANT on another type is reachable: the variant is a real way to
// call the tool, so the fact can be established without the deadlocked slot.
//
// The row exists because dropping the variant walk NARROWS the gate, which
// makes the refusal fire more often — a class that works being refused, with no
// override path. That is the expensive direction to fail in, and it is
// invisible to a mutation that flips the predicate rather than its input.
func TestReconcile_AdmitsAProducerWhoseVariantEscapesTheSlot(t *testing.T) {
	ac := requiringClass("factsrc-variant", "review_is_clean")
	tool := observingTool("check_review", gatedType, "review_is_clean", gatedType)
	tool.PermissionVariants = []authz.PermissionVariant{{
		When: `has(args.summary_only) && args.summary_only == true`, Check: *checkOn(unrelatedTyp),
	}}
	srv := declaringServer("factsrc-variant-srv", tool)
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{
		{Name: srv.Name, Ref: srv.Name},
	}

	got := reconcileClass(t, ac, srv)

	cond := validCondition(t, &got)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"a variant on another type is a reachable call; message=%s", cond.Message)

	e := sourceFor(got.Status.FactSources, "review_is_clean",
		"MCPServer/factsrc-variant-srv tool/check_review")
	require.NotNil(t, e, "%+v", got.Status.FactSources)
	assert.Equal(t, []string{gatedType, unrelatedTyp}, e.GatedBy,
		"the gate is the UNION of the base check and every variant, since which one runs "+
			"depends on argv and is not decidable at admission")
}

// A fact NOTHING on the class records is permanently undetermined too — but at
// admission that is indistinguishable from a fact a channel kind or an
// out-of-class tool legitimately supplies. So it is a WARNING on
// status.factSources, not a refusal, and the entry has to make the absence
// legible: an empty producedBy with no detail beside it would be the ambiguous
// shape this field exists to remove.
func TestReconcile_PublishesAFactNoToolProduces(t *testing.T) {
	ac := requiringClass("factsrc-no-producer", "review_is_clean")
	// The server carries a tool that observes something ELSE, so the class has
	// a producer surface and simply no producer for THIS fact.
	srv := declaringServer("factsrc-no-producer-srv",
		observingTool("check_review", unrelatedTyp, "some_other_fact", gatedType))
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{
		{Name: srv.Name, Ref: srv.Name},
	}

	got := reconcileClass(t, ac, srv)

	cond := validCondition(t, &got)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"an unproduced fact must not fail the class; message=%s", cond.Message)

	e := sourceFor(got.Status.FactSources, "review_is_clean", "")
	require.NotNil(t, e, "the absence must be visible: %+v", got.Status.FactSources)
	assert.Equal(t, "observed", e.Provenance)
	assert.Empty(t, e.GatedBy)
	assert.Equal(t, spiceboxv1alpha1.FactSourceStateAbsent, e.State,
		"the entry must SAY the absence is the finding, not leave an empty field to interpret")
	assert.NotEmpty(t, e.Detail, "the state is carried in prose for a person too")

	// The unrelated fact is NOT published: factSources answers "what do this
	// class's preconditions depend on", not "what does it observe".
	assert.Nil(t, sourceFor(got.Status.FactSources, "some_other_fact", ""),
		"a fact no precondition reads is not a dependency")
}

// A precondition over an ENVELOPE fact is published with an empty producedBy
// too, and must not read as the warning above: no tool ever records an envelope
// fact, so "nothing produces it" is the normal state rather than a finding.
func TestReconcile_PublishesAnEnvelopeFactAsItsOwnState(t *testing.T) {
	ac := newClass("factsrc-envelope")
	ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{Slots: []spiceboxv1alpha1.AuthzSlot{{
		ResourceType: gatedType,
		Description:  "a review the agent may act on",
		Permission:   "approve",
		// Gate-running fillFrom for the same reason as requiringClass: a gated
		// slot with an empty fillFrom admits channel_thread and is now refused.
		// The precondition here reads an envelope fact, but fillFrom governs how
		// the slot is FILLED, which is an orthogonal axis — observed is still the
		// gate-running route.
		FillFrom: []string{"observed"},
		Requires: []spiceboxv1alpha1.SlotPrecondition{{
			CEL:              "facts.envelope.head_is_fork == false",
			UndeterminedHint: "the delivery has not carried this yet",
			RefusalMessage:   "the change comes from a fork",
		}},
	}}}
	srv := declaringServer("factsrc-envelope-srv",
		observingTool("check_review", unrelatedTyp, "review_is_clean", gatedType))
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{
		{Name: srv.Name, Ref: srv.Name},
	}

	got := reconcileClass(t, ac, srv)

	cond := validCondition(t, &got)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "message=%s", cond.Message)

	e := sourceFor(got.Status.FactSources, "head_is_fork", "")
	require.NotNil(t, e, "%+v", got.Status.FactSources)
	assert.Equal(t, "envelope", e.Provenance)
	assert.Equal(t, spiceboxv1alpha1.FactSourceStateEnvelope, e.State,
		"an envelope fact has its own state; reading as the no-producer warning would "+
			"send an author looking for a producer that is not supposed to exist")
}

// A class with slots and NO tools at all takes neither of the two branches
// validatePermissions is called from, which is where a check placed in that
// function would silently stop applying. It must still publish its
// dependencies.
func TestReconcile_PublishesFactSourcesForAToollessClass(t *testing.T) {
	ac := requiringClass("factsrc-toolless", "review_is_clean")
	// No MCPServers, no toolBundles. The slot's standing comes from nowhere, so
	// the class is refused on THAT — but the observation is stamped first, and
	// this is exactly the shape a check hosted in validatePermissions would
	// never see.
	got := reconcileClass(t, ac)

	e := sourceFor(got.Status.FactSources, "review_is_clean", "")
	require.NotNil(t, e, "a class with no tools still depends on a fact: %+v", got.Status.FactSources)
	assert.Equal(t, spiceboxv1alpha1.FactSourceStateAbsent, e.State)
}

// Two producers, one blocked and one not: the fact is reachable, so the class
// admits. "Blocked" is a claim about EVERY way of establishing the fact, and a
// rule that fired on the first blocked producer would refuse a working class.
func TestReconcile_OneReachableProducerIsEnough(t *testing.T) {
	ac := requiringClass("factsrc-two-producers", "review_is_clean")
	srv := declaringServer("factsrc-two-producers-srv",
		observingTool("check_review", gatedType, "review_is_clean", gatedType),
		observingTool("scan_review", unrelatedTyp, "review_is_clean", gatedType))
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{
		{Name: srv.Name, Ref: srv.Name},
	}

	got := reconcileClass(t, ac, srv)

	cond := validCondition(t, &got)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"one reachable producer makes the fact establishable; message=%s", cond.Message)
	assert.Len(t, got.Status.FactSources, 2, "both producers are published: %+v", got.Status.FactSources)
}

// An UNGATED producer is reachable by definition, so it cannot close a cycle.
// It is a different state from "gate unknown" and the entry says so, because a
// reader deciding whether a precondition is answerable needs to tell an empty
// gatedBy that means "always allowed" from one that means "we could not tell".
func TestReconcile_AnUngatedProducerIsReachable(t *testing.T) {
	ac := requiringClass("factsrc-ungated", "review_is_clean")
	srv := declaringServer("factsrc-ungated-srv",
		observingTool("check_review", "", "review_is_clean", gatedType))
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{
		{Name: srv.Name, Ref: srv.Name},
	}

	got := reconcileClass(t, ac, srv)

	cond := validCondition(t, &got)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "message=%s", cond.Message)

	e := sourceFor(got.Status.FactSources, "review_is_clean",
		"MCPServer/factsrc-ungated-srv tool/check_review")
	require.NotNil(t, e, "%+v", got.Status.FactSources)
	assert.Empty(t, e.GatedBy)
	assert.Equal(t, spiceboxv1alpha1.FactSourceStateUngated, e.State,
		"an empty gatedBy that means ALWAYS ALLOWED must be distinguishable from one "+
			"that means the gate could not be resolved")
	assert.NotEmpty(t, e.Detail, "the state is carried in prose for a person too")
}

// ---------------------------------------------------------------------------
// The SECOND producer surface: a SpiceboxToolspec.
//
// A toolspec carries no permission of its own — the tool it becomes is gated by
// the TOOLKIT it narrows. Walking MCPServer tools alone would report a
// toolspec-produced fact as having no producer at all, which routes a genuine
// cycle to the warning path and admits the class. That is the silently-inert
// validation this whole check exists to prevent, so the toolspec surface gets
// its own end-to-end coverage rather than a unit test of the walk.
// ---------------------------------------------------------------------------

// sandboxFixture assembles the four objects a toolBundle-driven class needs: a
// SpiceboxClass naming the toolkit as a tool, the toolkit itself (gating each
// allowed subcommand on gateType), a Valid=True toolspec observing factName,
// and an MCPServer whose only job is to declare the standings.
//
// The toolkit declares the same standings as well as the server, so a class
// carrying toolBundles and NO MCPServers still resolves them — that is the
// fourth class shape, and it has its own row below.
//
// toolkitName is what the toolspec references. Pass a name with NO toolkit CR
// (and no embedded toolkit) to exercise the unresolvable-gate state.
//
// tweaks reshape the toolkit's permission surface, which is what the gate
// resolution reads: each row below says which part of that surface it is about
// rather than growing a parameter per part.
func sandboxFixture(
	prefix, toolkitName, gateType, factName string,
	withToolkitCR bool,
	tweaks ...func(*spiceboxv1alpha1.SpiceboxToolkit),
) []client.Object {
	cls := &spiceboxv1alpha1.SpiceboxClass{
		ObjectMeta: metav1.ObjectMeta{Name: prefix + "-sbx"},
		Spec: spiceboxv1alpha1.SpiceboxClassSpec{
			Tools: []spiceboxv1alpha1.SpiceboxTool{{
				Name: toolkitName, Command: []string{"/usr/bin/" + toolkitName},
			}},
		},
	}
	ts := &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: prefix + "-spec"},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Name:             prefix + "_tool",
			Toolkit:          spiceboxv1alpha1.ToolspecToolkitRef{Name: toolkitName, Revision: "1"},
			AllowSubcommands: []string{"review show"},
			Observes: []spiceboxv1alpha1.ObservesBlock{{
				Subjects: []spiceboxv1alpha1.ObserveSubject{{
					ResourceType: fmt.Sprintf("%q", gatedType),
					ResourceID:   "string(result.id)",
				}},
				Facts: map[string]string{factName: "result.clean"},
			}},
		},
		Status: spiceboxv1alpha1.SpiceboxToolspecStatus{Conditions: []metav1.Condition{{
			Type: spiceboxv1alpha1.SpiceboxToolspecConditionValid, Status: metav1.ConditionTrue,
			Reason: "OK", LastTransitionTime: metav1.Now(),
		}}},
	}
	objs := []client.Object{cls, ts, declaringServer(prefix + "-srv")}
	if !withToolkitCR {
		return objs
	}
	tk := &spiceboxv1alpha1.SpiceboxToolkit{
		ObjectMeta: metav1.ObjectMeta{Name: toolkitName},
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			Name:            toolkitName,
			ToolkitRevision: "1",
			SpiceDBSchema: &spiceboxv1alpha1.SpiceDBSchemaFragment{
				Resources: []spiceboxv1alpha1.SpiceDBResource{
					{Name: gatedType, Standing: spiceboxv1alpha1.StandingSessionOnly},
					{Name: unrelatedTyp, Standing: spiceboxv1alpha1.StandingSessionOnly},
				},
			},
			Subcommands: []spiceboxv1alpha1.ToolkitSubcommand{
				{
					Path:       []string{"review", "show"},
					Permission: checkOn(gateType),
				},
				{
					// Narrowed OUT by the toolspec's allowSubcommands, so its
					// permission is unreachable and must not widen the gate.
					// Without the narrowing this row would make every fixture
					// below look reachable and no cycle would ever be found.
					Path:       []string{"issue", "show"},
					Permission: checkOn(unrelatedTyp),
				},
			},
		},
	}
	for _, tweak := range tweaks {
		tweak(tk)
	}
	return append(objs, tk)
}

// checkOn is a permission whose check names one resource type.
func checkOn(resourceType string) *authz.Permission {
	return &authz.Permission{
		StateImpact: authz.Readonly,
		Check: &authz.PermissionCheck{
			ResourceType: resourceType, Permission: "read", ResourceIDTemplate: "{id}",
		},
	}
}

// withToolkitDefault gives the toolkit a toolkit-WIDE default permission. An
// argv matching no subcommand resolves to it, so it is part of the gate whether
// or not any subcommand declares one.
func withToolkitDefault(resourceType string) func(*spiceboxv1alpha1.SpiceboxToolkit) {
	return func(tk *spiceboxv1alpha1.SpiceboxToolkit) {
		tk.Spec.Permission = checkOn(resourceType)
	}
}

// withAllowedSubcommandVariant hangs an ARGUMENT-conditional permission variant
// off the one subcommand the toolspec admits.
func withAllowedSubcommandVariant(resourceType string) func(*spiceboxv1alpha1.SpiceboxToolkit) {
	return func(tk *spiceboxv1alpha1.SpiceboxToolkit) {
		for i := range tk.Spec.Subcommands {
			if strings.Join(tk.Spec.Subcommands[i].Path, " ") != "review show" {
				continue
			}
			tk.Spec.Subcommands[i].PermissionVariants = []authz.PermissionVariant{{
				When: `has(args.all) && args.all == "true"`, Check: *checkOn(resourceType),
			}}
		}
	}
}

// bundlesOnly drops the MCPServer from a sandbox fixture, leaving the fourth
// class shape: toolBundles and no MCPServers at all.
func bundlesOnly(objs []client.Object) []client.Object {
	out := make([]client.Object, 0, len(objs))
	for _, o := range objs {
		if _, isServer := o.(*spiceboxv1alpha1.MCPServer); isServer {
			continue
		}
		out = append(out, o)
	}
	return out
}

// bundleClass points a class at the sandbox fixture. userPassthrough so the
// bundle needs no AgentIdentity — this file is about facts, not credentials.
func bundleClass(name, factName, prefix string) *spiceboxv1alpha1.AgentClass {
	ac := requiringClass(name, factName)
	ac.Spec.IdentityMode = spiceboxv1alpha1.IdentityModeUserPassthrough
	ac.Spec.ToolBundles = []spiceboxv1alpha1.ToolBundle{{
		Name: "sandboxed", Class: prefix + "-sbx", Toolspecs: []string{prefix + "-spec"},
	}}
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{
		{Name: prefix + "-srv", Ref: prefix + "-srv"},
	}
	return ac
}

// The cycle closes through a TOOLSPEC producer exactly as it does through an
// MCP tool. If the producer walk missed this surface the class would admit,
// with the fact reported as having no producer at all.
func TestReconcile_RefusesACycleClosedByAToolspecProducer(t *testing.T) {
	const prefix = "factsrc-ts-cycle"
	objs := sandboxFixture(prefix, "review_cli", gatedType, "review_is_clean", true)
	ac := bundleClass(prefix, "review_is_clean", prefix)

	got := reconcileClass(t, ac, objs...)

	cond := validCondition(t, &got)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status,
		"a toolspec-produced fact must close the cycle too; message=%s", cond.Message)
	assert.Equal(t, spiceboxv1alpha1.ReasonSlotPreconditionUnsatisfiable, cond.Reason)
	assert.Contains(t, cond.Message, "the very slot this precondition holds shut")
	assert.Contains(t, cond.Message, "SpiceboxToolspec/"+prefix+"-spec",
		"the toolspec must be named as the producer, not reported as absent")

	e := sourceFor(got.Status.FactSources, "review_is_clean", "SpiceboxToolspec/"+prefix+"-spec")
	require.NotNil(t, e, "%+v", got.Status.FactSources)
	assert.Equal(t, []string{gatedType}, e.GatedBy,
		"only the ALLOWED subcommand's check gates this tool; the narrowed-out one must not widen it")
}

// The same fixture with the allowed subcommand gated elsewhere admits — which
// is what shows the toolspec gate is actually resolved through the toolkit
// rather than assumed.
func TestReconcile_AdmitsAToolspecProducerGatedElsewhere(t *testing.T) {
	const prefix = "factsrc-ts-ok"
	objs := sandboxFixture(prefix, "review_cli_b", unrelatedTyp, "review_is_clean", true)
	ac := bundleClass(prefix, "review_is_clean", prefix)

	got := reconcileClass(t, ac, objs...)

	cond := validCondition(t, &got)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, "message=%s", cond.Message)

	e := sourceFor(got.Status.FactSources, "review_is_clean", "SpiceboxToolspec/"+prefix+"-spec")
	require.NotNil(t, e, "%+v", got.Status.FactSources)
	assert.Equal(t, []string{unrelatedTyp}, e.GatedBy)
}

// P-9: a producer whose GATE could not be resolved is a third state. It is not
// "no producer" — the fact is recorded by something real — and it is not proof
// of a cycle either, so the class admits and the entry says what is unknown.
// Reporting it as an absent producer would understate what the walk knows;
// treating it as blocked would refuse a class on a guess.
func TestReconcile_ReportsAToolspecWhoseGateCannotBeResolved(t *testing.T) {
	const prefix = "factsrc-ts-unknown"
	// No SpiceboxToolkit CR, and no embedded toolkit of this name.
	objs := sandboxFixture(prefix, "no_such_toolkit", gatedType, "review_is_clean", false)
	ac := bundleClass(prefix, "review_is_clean", prefix)

	got := reconcileClass(t, ac, objs...)

	cond := validCondition(t, &got)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"an unresolved gate is not proof of a cycle; message=%s", cond.Message)

	e := sourceFor(got.Status.FactSources, "review_is_clean", "SpiceboxToolspec/"+prefix+"-spec")
	require.NotNil(t, e, "the producer is real and must be named: %+v", got.Status.FactSources)
	assert.Empty(t, e.GatedBy)
	assert.Equal(t, spiceboxv1alpha1.FactSourceStateUnresolved, e.State,
		"an unresolved gate must not be reported as an absent producer, nor as an ungated one")
	assert.Contains(t, e.Detail, "no_such_toolkit",
		"the prose must name the gate it could not resolve, not just that it failed")
}

// A toolkit's tool is reachable through the TOOLKIT-LEVEL default permission
// even when the one subcommand the toolspec admits is gated on the slot's own
// type: an argv matching no subcommand resolves to that default, so it is a way
// of calling the tool that the deadlocked slot does not gate.
//
// Same regression direction as the variant row above — dropping the default
// narrows the gate and refuses a class that works.
func TestReconcile_AdmitsAToolspecEscapingByItsToolkitDefault(t *testing.T) {
	const prefix = "factsrc-ts-default"
	objs := sandboxFixture(prefix, "review_cli_c", gatedType, "review_is_clean", true,
		withToolkitDefault(unrelatedTyp))
	ac := bundleClass(prefix, "review_is_clean", prefix)

	got := reconcileClass(t, ac, objs...)

	cond := validCondition(t, &got)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"the toolkit default is a reachable call this slot does not gate; message=%s", cond.Message)

	e := sourceFor(got.Status.FactSources, "review_is_clean", "SpiceboxToolspec/"+prefix+"-spec")
	require.NotNil(t, e, "%+v", got.Status.FactSources)
	assert.Equal(t, []string{gatedType, unrelatedTyp}, e.GatedBy,
		"the gate is the union of the allowed subcommand's check AND the toolkit default")
}

// The third sub-surface: a permission VARIANT on the allowed subcommand. The
// toolkit declares no default here, so the variant is the only escape, and
// dropping the variant walk over toolkit subcommands would refuse this class.
func TestReconcile_AdmitsAToolspecEscapingBySubcommandVariant(t *testing.T) {
	const prefix = "factsrc-ts-variant"
	objs := sandboxFixture(prefix, "review_cli_d", gatedType, "review_is_clean", true,
		withAllowedSubcommandVariant(unrelatedTyp))
	ac := bundleClass(prefix, "review_is_clean", prefix)

	got := reconcileClass(t, ac, objs...)

	cond := validCondition(t, &got)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"an argument variant on another type is a reachable call; message=%s", cond.Message)

	e := sourceFor(got.Status.FactSources, "review_is_clean", "SpiceboxToolspec/"+prefix+"-spec")
	require.NotNil(t, e, "%+v", got.Status.FactSources)
	assert.Equal(t, []string{gatedType, unrelatedTyp}, e.GatedBy,
		"a subcommand's variants widen its gate exactly as an MCP tool's do")
}

// The FOURTH class shape: toolBundles and NO MCPServers, which is the branch
// validatePermissions is called from with nil servers. A check hosted there
// would still run — but a check hosted in the OTHER branch would not, and this
// row is what says which. Standings come from the toolkit's own schema
// fragment, since there is no server to declare them.
func TestReconcile_RefusesACycleOnABundlesOnlyClass(t *testing.T) {
	const prefix = "factsrc-bundles-only"
	objs := bundlesOnly(sandboxFixture(prefix, "review_cli_e", gatedType, "review_is_clean", true))
	ac := bundleClass(prefix, "review_is_clean", prefix)
	ac.Spec.MCPServers = nil

	got := reconcileClass(t, ac, objs...)

	cond := validCondition(t, &got)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status,
		"the cycle must be found on a class with no MCPServers at all; message=%s", cond.Message)
	assert.Equal(t, spiceboxv1alpha1.ReasonSlotPreconditionUnsatisfiable, cond.Reason)
	assert.Contains(t, cond.Message, "the very slot this precondition holds shut")

	e := sourceFor(got.Status.FactSources, "review_is_clean", "SpiceboxToolspec/"+prefix+"-spec")
	require.NotNil(t, e, "%+v", got.Status.FactSources)
	assert.Equal(t, []string{gatedType}, e.GatedBy)
}

// One tool recording the same fact from TWO observes blocks — different
// subjects, different conditions — is one producer of it, not two. The walk
// collects fact names across every block, so without a dedupe the class
// publishes two byte-identical rows for one call.
func TestReconcile_PublishesOneEntryPerProducerNotPerObservesBlock(t *testing.T) {
	ac := requiringClass("factsrc-dup", "review_is_clean")
	tool := observingTool("check_review", unrelatedTyp, "review_is_clean", gatedType)
	// A second block over the same fact, keyed on a different subject — the
	// shape a tool takes when one response describes two things.
	tool.Observes = append(tool.Observes, spiceboxv1alpha1.ObservesBlock{
		Subjects: []spiceboxv1alpha1.ObserveSubject{{
			ResourceType: fmt.Sprintf("%q", unrelatedTyp),
			ResourceID:   "string(result.issue_id)",
		}},
		Facts: map[string]string{"review_is_clean": "result.clean"},
	})
	srv := declaringServer("factsrc-dup-srv", tool)
	ac.Spec.MCPServers = []spiceboxv1alpha1.AgentClassMCPServerRef{
		{Name: srv.Name, Ref: srv.Name},
	}

	got := reconcileClass(t, ac, srv)

	assert.Len(t, got.Status.FactSources, 1,
		"two observes blocks on one tool are one producer: %+v", got.Status.FactSources)
	// Len alone is satisfied by the one row a class with NO producers publishes
	// (state absent), so it would stay green if the walk lost every producer —
	// the opposite of what this row claims. Naming the producer is what makes
	// the count mean "deduped to one" rather than "collapsed to none".
	e := sourceFor(got.Status.FactSources, "review_is_clean",
		"MCPServer/factsrc-dup-srv tool/check_review")
	require.NotNil(t, e, "%+v", got.Status.FactSources)
	assert.Equal(t, spiceboxv1alpha1.FactSourceStateGated, e.State)
}
