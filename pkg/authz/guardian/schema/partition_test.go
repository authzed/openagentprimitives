package schema_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/schema"
)

// resourceFragment builds a one-resource fragment: resource `name` with a
// single relation `rel: user` and a single permission `<perm> = rel`.
// Two fragments sharing a resource name but differing in perm/rel are a
// cross-fragment conflict (EmitSpicedbSchema "conflicting definitions").
func resourceFragment(name, rel, perm string) *spiceboxv1alpha1.SpiceDBSchemaFragment {
	return &spiceboxv1alpha1.SpiceDBSchemaFragment{
		Resources: []spiceboxv1alpha1.SpiceDBResource{{
			Name:        name,
			Relations:   []spiceboxv1alpha1.SpiceDBRelation{{Name: rel, SubjectType: "user"}},
			Permissions: []spiceboxv1alpha1.SpiceDBPermission{{Name: perm, Expr: rel}},
		}},
	}
}

func keysOf(frags []schema.IdentifiedFragment) []string {
	out := make([]string, 0, len(frags))
	for _, f := range frags {
		out = append(out, f.Key)
	}
	return out
}

func rejectedKeysOf(rej []schema.RejectedFragment) []string {
	out := make([]string, 0, len(rej))
	for _, r := range rej {
		out = append(out, r.Key)
	}
	return out
}

// TestPartitionCompatibleFragments_IsolatesConflict is the core N-way
// case: A (resource x), B (resource x with a DIFFERENT body → conflicts
// with A), C (unrelated resource z). Sorted order is A,B,C, so A wins the
// x-conflict and B is rejected; C is accepted. This is the maximal
// conflict-free subset — RunAll over it can never freeze.
func TestPartitionCompatibleFragments_IsolatesConflict(t *testing.T) {
	a := schema.IdentifiedFragment{Key: "ns/a", Fragment: resourceFragment("shared_res", "reader", "read")}
	b := schema.IdentifiedFragment{Key: "ns/b", Fragment: resourceFragment("shared_res", "writer", "write")} // conflicts with a
	c := schema.IdentifiedFragment{Key: "ns/c", Fragment: resourceFragment("other_res", "reader", "read")}

	accepted, rejected := schema.PartitionCompatibleFragments(nil, []schema.IdentifiedFragment{a, b, c})

	assert.Equal(t, []string{"ns/a", "ns/c"}, keysOf(accepted), "A and C accepted; B (later conflict) rejected")
	assert.Equal(t, []string{"ns/b"}, rejectedKeysOf(rejected), "only B rejected")

	// The accepted set MUST compose — that is the guarantee that prevents
	// the global freeze downstream in RunAll.
	_, err := schema.ComposeAll(accepted, nil, nil)
	assert.NoError(t, err, "the accepted subset must compose cleanly")
}

// TestPartitionCompatibleFragments_DisplacedByNamesTheWinner is the design
// §3 case: the rejected fragment's RejectedFragment.DisplacedBy names the
// SPECIFIC already-accepted fragment whose presence caused the rejection —
// not just that A conflict happened, but WHICH contributor won it. Same A/B
// pair as TestPartitionCompatibleFragments_IsolatesConflict, asserting the
// field that test doesn't touch.
func TestPartitionCompatibleFragments_DisplacedByNamesTheWinner(t *testing.T) {
	a := schema.IdentifiedFragment{Key: "ns/a", Fragment: resourceFragment("shared_res", "reader", "read")}
	b := schema.IdentifiedFragment{Key: "ns/b", Fragment: resourceFragment("shared_res", "writer", "write")} // conflicts with a
	c := schema.IdentifiedFragment{Key: "ns/c", Fragment: resourceFragment("other_res", "reader", "read")}

	_, rejected := schema.PartitionCompatibleFragments(nil, []schema.IdentifiedFragment{a, b, c})

	require.Len(t, rejected, 1)
	assert.Equal(t, "ns/b", rejected[0].Key)
	assert.Equal(t, "ns/a", rejected[0].DisplacedBy,
		"DisplacedBy names A specifically — C is accepted too but never contended for shared_res")
}

// TestPartitionCompatibleFragments_DisplacedByJoinsIdenticalWinners covers
// the case RejectedFragment.DisplacedBy's doc comment calls out: several
// already-accepted fragments can declare BYTE-IDENTICAL bodies for the name
// a later candidate collides on (EmitSpicedbSchema permits identical
// redeclaration — only a mismatch conflicts), so more than one of them
// reproduces the candidate's failure alone. DisplacedBy must name all of
// them rather than picking one arbitrarily and hiding the rest.
func TestPartitionCompatibleFragments_DisplacedByJoinsIdenticalWinners(t *testing.T) {
	a := schema.IdentifiedFragment{Key: "ns/a", Fragment: resourceFragment("shared_res", "reader", "read")}
	b := schema.IdentifiedFragment{Key: "ns/b", Fragment: resourceFragment("shared_res", "reader", "read")}  // identical to a — no conflict between them
	c := schema.IdentifiedFragment{Key: "ns/c", Fragment: resourceFragment("shared_res", "writer", "write")} // conflicts with BOTH a and b

	accepted, rejected := schema.PartitionCompatibleFragments(nil, []schema.IdentifiedFragment{a, b, c})

	require.Equal(t, []string{"ns/a", "ns/b"}, keysOf(accepted), "a and b are byte-identical, so both compose")
	require.Len(t, rejected, 1)
	assert.Equal(t, "ns/c", rejected[0].Key)
	assert.Equal(t, "ns/a, ns/b", rejected[0].DisplacedBy,
		"both accepted fragments reproduce c's conflict alone — DisplacedBy names both, sorted")
}

// TestPartitionCompatibleFragments_Deterministic verifies the partition
// depends only on the fragments' keys, not on the order the caller
// supplied them — the property that makes the downstream schema write
// idempotent regardless of cluster List() ordering. Every permutation of
// the same three fragments yields accepted [A,C] / rejected [B].
func TestPartitionCompatibleFragments_Deterministic(t *testing.T) {
	a := schema.IdentifiedFragment{Key: "ns/a", Fragment: resourceFragment("shared_res", "reader", "read")}
	b := schema.IdentifiedFragment{Key: "ns/b", Fragment: resourceFragment("shared_res", "writer", "write")}
	c := schema.IdentifiedFragment{Key: "ns/c", Fragment: resourceFragment("other_res", "reader", "read")}

	perms := [][]schema.IdentifiedFragment{
		{a, b, c},
		{c, b, a},
		{b, c, a},
		{c, a, b},
	}
	for _, in := range perms {
		accepted, rejected := schema.PartitionCompatibleFragments(nil, in)
		assert.Equalf(t, []string{"ns/a", "ns/c"}, keysOf(accepted), "accepted must be order-independent for input %v", keysOf(in))
		assert.Equalf(t, []string{"ns/b"}, rejectedKeysOf(rejected), "rejected must be order-independent for input %v", keysOf(in))
	}
}

// TestPartitionCompatibleFragments_TieBreakFirstWins pins the tie-break
// direction: with two mutually-conflicting fragments and NOTHING else,
// the lexicographically-earlier key is accepted and the later is
// rejected — never both (rejecting both would let a hostile tenant knock
// out a specific victim).
func TestPartitionCompatibleFragments_TieBreakFirstWins(t *testing.T) {
	early := schema.IdentifiedFragment{Key: "ns/aaa", Fragment: resourceFragment("shared_res", "reader", "read")}
	late := schema.IdentifiedFragment{Key: "ns/zzz", Fragment: resourceFragment("shared_res", "writer", "write")}

	accepted, rejected := schema.PartitionCompatibleFragments(nil, []schema.IdentifiedFragment{late, early})
	require.Len(t, accepted, 1, "exactly one side of the conflict is accepted, never zero")
	assert.Equal(t, "ns/aaa", accepted[0].Key, "the earlier key wins")
	require.Len(t, rejected, 1, "the later key is the only rejection")
	assert.Equal(t, "ns/zzz", rejected[0].Key, "the later key is rejected")
}

// TestPartitionCompatibleFragments_AllCompatible verifies the no-conflict
// case is a pure pass-through: every fragment is accepted, none rejected.
func TestPartitionCompatibleFragments_AllCompatible(t *testing.T) {
	a := schema.IdentifiedFragment{Key: "ns/a", Fragment: resourceFragment("shared_res", "reader", "read")}
	c := schema.IdentifiedFragment{Key: "ns/c", Fragment: resourceFragment("other_res", "reader", "read")}

	accepted, rejected := schema.PartitionCompatibleFragments(nil, []schema.IdentifiedFragment{a, c})
	assert.Equal(t, []string{"ns/a", "ns/c"}, keysOf(accepted), "both accepted")
	assert.Empty(t, rejected, "no rejections when nothing conflicts")
}

// TestPartition_OperatorTierWinsAgainstAnEarlierSortingTenant is the
// tier-first RED/GREEN case: tie-break is by TIER first, then by key.
// Without the tier an agent-authored fragment displaces an
// operator-authored one whenever it happens to sort earlier, which is an
// accident of naming deciding who owns a resource type.
func TestPartition_OperatorTierWinsAgainstAnEarlierSortingTenant(t *testing.T) {
	operator := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		RawZed: "definition contested {\n    relation owner: user\n    permission read = owner\n}\n",
	}
	tenant := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		RawZed: "definition contested {\n    relation viewer: user\n    permission read = viewer\n}\n",
	}

	// The tenant sorts FIRST lexicographically and must still lose.
	accepted, rejected := schema.PartitionCompatibleFragments(nil, []schema.IdentifiedFragment{
		{Key: "aaa-tenant", Tier: schema.TierTenant, Fragment: tenant},
		{Key: "zzz-operator", Tier: schema.TierOperator, Fragment: operator},
	})

	require.Len(t, accepted, 1)
	assert.Equal(t, "zzz-operator", accepted[0].Key, "operator-authored outranks tenant-authored")
	require.Len(t, rejected, 1)
	assert.Equal(t, "aaa-tenant", rejected[0].Key)
}

// TestPartition_WithinATierKeyStillDecides verifies that within one tier
// the existing lexicographic order still decides, because the accepted set
// feeds an idempotent schema write and must not depend on List() ordering.
func TestPartition_WithinATierKeyStillDecides(t *testing.T) {
	a := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		RawZed: "definition contested {\n    relation owner: user\n    permission read = owner\n}\n",
	}
	b := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		RawZed: "definition contested {\n    relation viewer: user\n    permission read = viewer\n}\n",
	}

	accepted, rejected := schema.PartitionCompatibleFragments(nil, []schema.IdentifiedFragment{
		{Key: "b-second", Tier: schema.TierTenant, Fragment: b},
		{Key: "a-first", Tier: schema.TierTenant, Fragment: a},
	})

	require.Len(t, accepted, 1)
	assert.Equal(t, "a-first", accepted[0].Key)
	require.Len(t, rejected, 1)
}

// TestComposeAll_ConflictingSurvivorsFreeze is the RED-confirm for the
// residual this partition closes: two fragments that each pass
// ValidateFragment ALONE but declare the same resource name with
// different bodies STILL fail ComposeAll when composed together — i.e.
// fed unfiltered into RunAll (the pre-partition behavior) they freeze the
// whole batch. PartitionCompatibleFragments (above) is what stops that
// from ever reaching RunAll.
func TestComposeAll_ConflictingSurvivorsFreeze(t *testing.T) {
	a := resourceFragment("shared_res", "reader", "read")
	b := resourceFragment("shared_res", "writer", "write") // same name, different body
	c := resourceFragment("other_res", "reader", "read")

	// Each is individually valid…
	require.NoError(t, schema.ValidateFragment(a), "A valid alone")
	require.NoError(t, schema.ValidateFragment(b), "B valid alone")
	require.NoError(t, schema.ValidateFragment(c), "C valid alone")

	// …yet composing all three at once (old un-partitioned RunAll input) errors.
	_, err := schema.ComposeAll(identify(a, b, c), nil, nil)
	require.Error(t, err, "conflicting survivors must fail a combined compose — the freeze the partition prevents")
	assert.Contains(t, err.Error(), "conflicting definitions", "error names the cross-fragment conflict")
}

// A tenant fragment that collides with a CHANNEL-KIND definition must be
// isolated by the partition — not accepted here and then left to freeze the
// whole cluster's schema write in RunAll.
//
// The isolation trials compose only the tenant candidates against each other;
// the channel-kind fragments (slack_channel, string, agent, github_user, …)
// were passed as nil, so a tenant fragment redeclaring one of those names
// passed the partition. RunAll then merges both and compiler.Compile fails on
// the duplicate → WriteSchema never runs → every AgentSessionGrants in the
// cluster stalls, while the offending CR is marked valid. The partition's own
// invariant ("the last successful ComposeAll ran over exactly the final
// accepted set, so RunAll is guaranteed to compose") is false unless the trials
// see the SAME baseline RunAll does.
func TestPartitionCompatibleFragments_IsolatesAChannelKindCollision(t *testing.T) {
	// The channel-kind baseline RunAll composes against — highest trust,
	// always present, never a candidate.
	baseline := []*spiceboxv1alpha1.SpiceDBSchemaFragment{
		resourceFragment("slack_channel", "reader", "read"),
	}
	// A tenant fragment redeclaring slack_channel with a different body.
	tenant := schema.IdentifiedFragment{
		Key:      "tenant-ns/evil",
		Tier:     schema.TierTenant,
		Fragment: resourceFragment("slack_channel", "writer", "write"),
	}
	// An honest tenant fragment on its own name, to prove the baseline is a
	// filter and not a wall.
	ok := schema.IdentifiedFragment{
		Key:      "tenant-ns/ok",
		Tier:     schema.TierTenant,
		Fragment: resourceFragment("tenant_widget", "reader", "read"),
	}

	accepted, rejected := schema.PartitionCompatibleFragments(baseline,
		[]schema.IdentifiedFragment{tenant, ok})

	assert.Equal(t, []string{"tenant-ns/ok"}, keysOf(accepted),
		"the honest fragment is accepted; the channel-kind collider is not")
	assert.Equal(t, []string{"tenant-ns/evil"}, rejectedKeysOf(rejected),
		"a fragment colliding with a channel-kind definition must be isolated to its own CR")

	// And the guarantee that actually matters: the accepted set composed
	// against the SAME baseline RunAll uses must not fail.
	_, err := schema.ComposeAll(accepted, baseline, nil)
	assert.NoError(t, err, "accepted set + channel-kind baseline must compose — that is what stops the freeze")
}

// rawZedFrag is a fragment defined by raw zed text (for arrow shapes
// resourceFragment cannot express).
func rawZedFrag(zed string) *spiceboxv1alpha1.SpiceDBSchemaFragment {
	return &spiceboxv1alpha1.SpiceDBSchemaFragment{RawZed: zed}
}

// A tenant fragment with a dangling permission reference onto a CHANNEL-KIND
// (baseline) type must be isolated by the partition — not accepted and then
// left to freeze the whole cluster's schema write in RunAll.
//
// ValidateFragment composes the fragment over the SCAFFOLD ONLY, so an arrow
// onto a baseline type is skipped there (target absent). The partition is where
// the baseline is present, so it is the only place this can be ISOLATED to
// just the offending fragment — RunAll's own ValidateComposedSchema gate would
// still catch it, in-process, before WriteSchema is ever called, but only by
// refusing the ENTIRE write, with no single fragment to blame. Without the
// partition catching it first, the merged schema fails that gate wholesale,
// and every AgentSessionGrants stalls.
func TestPartitionCompatibleFragments_IsolatesADanglingRefOntoTheBaseline(t *testing.T) {
	// A channel-kind baseline type the tenant fragment can reference.
	baseline := []*spiceboxv1alpha1.SpiceDBSchemaFragment{
		rawZedFrag("definition basetype {\n\trelation member: user\n}\n"),
	}
	// Arrows onto basetype's nonexistent permission — a dangling reference that
	// only resolves against the baseline, so scaffold-only validation misses it.
	evil := schema.IdentifiedFragment{
		Key:      "tenant-ns/evil",
		Tier:     schema.TierTenant,
		Fragment: rawZedFrag("definition evil_res {\n\trelation source: basetype\n\tpermission viewx = source->nonexistent\n}\n"),
	}
	honest := schema.IdentifiedFragment{
		Key:      "tenant-ns/ok",
		Tier:     schema.TierTenant,
		Fragment: resourceFragment("tenant_widget", "reader", "read"),
	}

	accepted, rejected := schema.PartitionCompatibleFragments(baseline,
		[]schema.IdentifiedFragment{evil, honest})

	assert.Equal(t, []string{"tenant-ns/ok"}, keysOf(accepted),
		"the honest fragment is accepted; the dangling-ref fragment is not")
	assert.Equal(t, []string{"tenant-ns/evil"}, rejectedKeysOf(rejected),
		"a fragment with a dangling reference onto a baseline type must be isolated to its own CR")
}

// Resolving a direct relation's SUBJECT TYPES made the partition
// order-dependent, and this is that regression. The walk judges each candidate
// against the baseline plus the already-accepted prefix, so a fragment typed
// onto a SIBLING candidate's definition looks dangling whenever the sibling
// sorts after it — even though the pair composes cleanly at the real
// WriteSchema, which is the only thing that matters.
//
// The fix is the FLOOR, never the check. Skipping SubjectType findings here
// would be skipping them at the one layer that enforces them, and a genuinely
// dangling tenant type would then reach SpiceDB and freeze the cluster-wide
// schema — which is the entire reason this partition exists.
func TestPartition_ASiblingDeclaredTypeIsNotTheCandidatesFault(t *testing.T) {
	// Keys chosen so the CONSUMER sorts first: it is judged before the
	// provider it depends on has been accepted.
	consumer := schema.IdentifiedFragment{
		Key:      "tenant-ns/aaa-consumer",
		Tier:     schema.TierTenant,
		Fragment: rawZedFrag("definition aaa_consumer {\n\trelation direct_member: zz_provider#member\n\tpermission members = direct_member\n}\n"),
	}
	provider := schema.IdentifiedFragment{
		Key:      "tenant-ns/zzz-provider",
		Tier:     schema.TierTenant,
		Fragment: rawZedFrag("definition zz_provider {\n\trelation member: user\n}\n"),
	}

	accepted, rejected := schema.PartitionCompatibleFragments(nil,
		[]schema.IdentifiedFragment{consumer, provider})

	assert.Empty(t, rejectedKeysOf(rejected),
		"a reference that resolves once every sibling is present is nobody's dangling reference")
	assert.ElementsMatch(t, []string{"tenant-ns/aaa-consumer", "tenant-ns/zzz-provider"}, keysOf(accepted))
}

// The order-independence half of the same claim: swapping which one sorts
// first must not change the verdict. A partition whose answer depends on a
// map-key sort is a partition that fails intermittently in production.
func TestPartition_SiblingResolutionIsOrderIndependent(t *testing.T) {
	provider := schema.IdentifiedFragment{
		Key:      "tenant-ns/aaa-provider",
		Tier:     schema.TierTenant,
		Fragment: rawZedFrag("definition zz_provider {\n\trelation member: user\n}\n"),
	}
	consumer := schema.IdentifiedFragment{
		Key:      "tenant-ns/zzz-consumer",
		Tier:     schema.TierTenant,
		Fragment: rawZedFrag("definition aaa_consumer {\n\trelation direct_member: zz_provider#member\n\tpermission members = direct_member\n}\n"),
	}

	accepted, rejected := schema.PartitionCompatibleFragments(nil,
		[]schema.IdentifiedFragment{provider, consumer})

	assert.Empty(t, rejectedKeysOf(rejected))
	assert.Len(t, accepted, 2)
}

// The floor must widen only as far as the candidates actually reach. A type NO
// candidate declares is still dangling with every sibling present, so it is
// still the candidate's doing and is still isolated to its own CR — otherwise
// the widened floor would have quietly turned this check off.
func TestPartition_ATypeNoCandidateDeclaresIsStillRejected(t *testing.T) {
	orphan := schema.IdentifiedFragment{
		Key:      "tenant-ns/aaa-orphan",
		Tier:     schema.TierTenant,
		Fragment: rawZedFrag("definition aaa_orphan {\n\trelation direct_member: nosuch_provider#member\n\tpermission members = direct_member\n}\n"),
	}
	honest := schema.IdentifiedFragment{
		Key:      "tenant-ns/zzz-ok",
		Tier:     schema.TierTenant,
		Fragment: resourceFragment("tenant_widget", "reader", "read"),
	}

	accepted, rejected := schema.PartitionCompatibleFragments(nil,
		[]schema.IdentifiedFragment{orphan, honest})

	assert.Equal(t, []string{"tenant-ns/aaa-orphan"}, rejectedKeysOf(rejected),
		"nothing declares nosuch_provider, so this reference would reach SpiceDB and freeze the schema")
	assert.Equal(t, []string{"tenant-ns/zzz-ok"}, keysOf(accepted))
}

// A sibling declaring the SUBJECT SET's relation is the other half: the type
// resolving is not enough if the named relation does not exist on it.
func TestPartition_ASiblingTypeMissingTheNamedRelationIsStillRejected(t *testing.T) {
	consumer := schema.IdentifiedFragment{
		Key:      "tenant-ns/aaa-consumer",
		Tier:     schema.TierTenant,
		Fragment: rawZedFrag("definition aaa_consumer {\n\trelation direct_member: zz_provider#nosuchrel\n}\n"),
	}
	provider := schema.IdentifiedFragment{
		Key:      "tenant-ns/zzz-provider",
		Tier:     schema.TierTenant,
		Fragment: rawZedFrag("definition zz_provider {\n\trelation member: user\n}\n"),
	}

	accepted, rejected := schema.PartitionCompatibleFragments(nil,
		[]schema.IdentifiedFragment{consumer, provider})

	assert.Equal(t, []string{"tenant-ns/aaa-consumer"}, rejectedKeysOf(rejected),
		"zz_provider exists but declares no nosuchrel, which SpiceDB rejects exactly as hard")
	assert.Equal(t, []string{"tenant-ns/zzz-provider"}, keysOf(accepted))
}

// When the all-candidates compose FAILS — two candidates collide, which is the
// ordinary case this partition exists for — there is no widened floor to
// compute, and the fallback must be today's behavior rather than an empty floor
// that accepts everything. The colliding pair still partitions, and a dangling
// reference in the same batch is still caught.
func TestPartition_FallsBackWhenTheAllCandidatesComposeFails(t *testing.T) {
	a := schema.IdentifiedFragment{Key: "ns/a", Fragment: resourceFragment("shared_res", "reader", "read")}
	b := schema.IdentifiedFragment{Key: "ns/b", Fragment: resourceFragment("shared_res", "writer", "write")}
	dangling := schema.IdentifiedFragment{
		Key:      "ns/c",
		Fragment: rawZedFrag("definition c_res {\n\trelation direct_member: nosuch_provider#member\n}\n"),
	}

	accepted, rejected := schema.PartitionCompatibleFragments(nil,
		[]schema.IdentifiedFragment{a, b, dangling})

	assert.Equal(t, []string{"ns/a"}, keysOf(accepted))
	assert.ElementsMatch(t, []string{"ns/b", "ns/c"}, rejectedKeysOf(rejected),
		"a failed all-candidates compose must not disable the dangling-reference check")
}

// assertAcceptedResolves is the partition's whole contract to RunAll, as an
// assertion: the accepted set, composed against the same baseline RunAll uses,
// names nothing it does not declare. RunAll writes that composition, and its
// own ValidateComposedSchema gate would refuse anything left dangling here
// in-process, before WriteSchema is ever called — freezing the schema for
// every AgentSessionGrants in the cluster all the same, since the gate has no
// single fragment to isolate at that point.
//
// Pre-existing baseline findings are subtracted, exactly as the partition
// subtracts them: a code-owned baseline that already carried one is not
// something any candidate can be blamed for, or asked to fix.
func assertAcceptedResolves(t *testing.T, baseline []*spiceboxv1alpha1.SpiceDBSchemaFragment, accepted []schema.IdentifiedFragment) {
	t.Helper()

	floor := map[schema.UnresolvedReference]struct{}{}
	if base, err := schema.ComposeAll(nil, baseline, nil); err == nil {
		if refs, rerr := schema.UnresolvedReferences(base); rerr == nil {
			for _, r := range refs {
				floor[r] = struct{}{}
			}
		}
	}

	composed, err := schema.ComposeAll(accepted, baseline, nil)
	require.NoError(t, err, "the accepted set must compose at all")

	refs, err := schema.UnresolvedReferences(composed)
	require.NoError(t, err)

	var dangling []string
	for _, r := range refs {
		if _, pre := floor[r]; pre {
			continue
		}
		dangling = append(dangling, r.String())
	}
	assert.Empty(t, dangling, "the accepted set must resolve; these would reach WriteSchema: %v", dangling)
}

// The residual wave 2 left open, closed by the convergence post-pass.
//
// The provider is rejected for a reason of its OWN — it carries a second
// definition whose reference dangles no matter who is present — and removing it
// takes `zz_provider` out of the accepted set with it. The consumer was judged
// while the provider was still a candidate, so its reference resolved then and
// it was (correctly, at that moment) accepted. Nothing in the incremental walk
// revisits that decision, so the consumer stayed accepted holding a reference
// to a definition no longer in the set — and RunAll would have written it.
//
// The blame must be the DANGLING one, naming the reference. Reporting the
// provider's own failure against the consumer's CR would send an operator to
// read a fragment they did not write.
func TestPartition_AConsumerIsRejectedWhenItsProviderIsRejected(t *testing.T) {
	consumer := schema.IdentifiedFragment{
		Key:      "tenant-ns/aaa-consumer",
		Tier:     schema.TierTenant,
		Fragment: rawZedFrag("definition aaa_consumer {\n\trelation direct_member: zz_provider#member\n}\n"),
	}
	// Declares the type the consumer needs, AND a second definition whose own
	// reference resolves to nothing with every candidate present — so the
	// pre-rejection settles it out of the set.
	provider := schema.IdentifiedFragment{
		Key:  "tenant-ns/zzz-provider",
		Tier: schema.TierTenant,
		Fragment: rawZedFrag(
			"definition zz_provider {\n\trelation member: user\n}\n" +
				"definition zz_broken {\n\trelation bad: nosuch_type#member\n}\n"),
	}

	accepted, rejected := schema.PartitionCompatibleFragments(nil,
		[]schema.IdentifiedFragment{consumer, provider})

	assert.Empty(t, keysOf(accepted),
		"the consumer cannot stay accepted once the definition it types onto has left the set")
	assert.ElementsMatch(t, []string{"tenant-ns/aaa-consumer", "tenant-ns/zzz-provider"}, rejectedKeysOf(rejected))

	var consumerErr string
	for _, r := range rejected {
		if r.Key == "tenant-ns/aaa-consumer" {
			consumerErr = r.Err.Error()
		}
	}
	require.NotEmpty(t, consumerErr)
	assert.Contains(t, consumerErr, "zz_provider",
		"the consumer's rejection must name ITS OWN dangling reference, not the provider's unrelated failure")
	assert.NotContains(t, consumerErr, "nosuch_type",
		"the provider's own broken reference is not the consumer author's problem to read")

	assertAcceptedResolves(t, nil, accepted)
}

// A survivor that does not depend on the removed fragment must be kept. The
// post-pass removes what dangles, never everything downstream of a rejection.
func TestPartition_ConvergencePassKeepsTheIndependentSurvivors(t *testing.T) {
	consumer := schema.IdentifiedFragment{
		Key:      "tenant-ns/aaa-consumer",
		Tier:     schema.TierTenant,
		Fragment: rawZedFrag("definition aaa_consumer {\n\trelation direct_member: zz_provider#member\n}\n"),
	}
	provider := schema.IdentifiedFragment{
		Key:  "tenant-ns/mmm-provider",
		Tier: schema.TierTenant,
		Fragment: rawZedFrag(
			"definition zz_provider {\n\trelation member: user\n}\n" +
				"definition zz_broken {\n\trelation bad: nosuch_type#member\n}\n"),
	}
	independent := schema.IdentifiedFragment{
		Key:      "tenant-ns/zzz-ok",
		Tier:     schema.TierTenant,
		Fragment: resourceFragment("tenant_widget", "reader", "read"),
	}

	accepted, rejected := schema.PartitionCompatibleFragments(nil,
		[]schema.IdentifiedFragment{consumer, provider, independent})

	assert.Equal(t, []string{"tenant-ns/zzz-ok"}, keysOf(accepted),
		"the fragment that depends on nothing removed must survive")
	assert.ElementsMatch(t, []string{"tenant-ns/aaa-consumer", "tenant-ns/mmm-provider"}, rejectedKeysOf(rejected))

	assertAcceptedResolves(t, nil, accepted)
}

// A chain: C types onto B's definition, B types onto A's, A is the one with the
// independently dangling reference. Removing A must cascade through B to C,
// which is why the post-pass repeats rather than running once. It terminates
// because every iteration removes at least one fragment.
func TestPartition_ConvergencePassCascadesThroughAChain(t *testing.T) {
	root := schema.IdentifiedFragment{
		Key:  "tenant-ns/a-root",
		Tier: schema.TierTenant,
		Fragment: rawZedFrag(
			"definition r_root {\n\trelation member: user\n}\n" +
				"definition r_broken {\n\trelation bad: nosuch_type#member\n}\n"),
	}
	mid := schema.IdentifiedFragment{
		Key:      "tenant-ns/b-mid",
		Tier:     schema.TierTenant,
		Fragment: rawZedFrag("definition r_mid {\n\trelation upref: r_root#member\n}\n"),
	}
	leaf := schema.IdentifiedFragment{
		Key:      "tenant-ns/c-leaf",
		Tier:     schema.TierTenant,
		Fragment: rawZedFrag("definition r_leaf {\n\trelation upref: r_mid#upref\n}\n"),
	}

	accepted, rejected := schema.PartitionCompatibleFragments(nil,
		[]schema.IdentifiedFragment{root, mid, leaf})

	assert.Empty(t, keysOf(accepted), "the whole chain hangs off the removed root")
	assert.ElementsMatch(t,
		[]string{"tenant-ns/a-root", "tenant-ns/b-mid", "tenant-ns/c-leaf"},
		rejectedKeysOf(rejected))

	assertAcceptedResolves(t, nil, accepted)
}

// The post-pass filters `accepted` in place, so a clean set must come back
// byte-identical — same members, same ORDER. Order is load-bearing: RunAll
// composes this slice, and the partition's own tie-breaking (higher tier first,
// then Key) is what makes rejections attributable.
func TestPartition_ConvergencePassLeavesACleanSetUntouched(t *testing.T) {
	baseline := []*spiceboxv1alpha1.SpiceDBSchemaFragment{
		rawZedFrag("definition basetype {\n\trelation member: user\n}\n"),
	}
	operator := schema.IdentifiedFragment{
		Key:      "op-ns/zzz-late-key",
		Tier:     schema.TierOperator,
		Fragment: resourceFragment("op_widget", "reader", "read"),
	}
	tenantA := schema.IdentifiedFragment{
		Key:      "tenant-ns/aaa",
		Tier:     schema.TierTenant,
		Fragment: rawZedFrag("definition t_a {\n\trelation src: basetype\n\tpermission view = src->member\n}\n"),
	}
	tenantB := schema.IdentifiedFragment{
		Key:      "tenant-ns/bbb",
		Tier:     schema.TierTenant,
		Fragment: rawZedFrag("definition t_b {\n\trelation peer: t_a#src\n}\n"),
	}

	accepted, rejected := schema.PartitionCompatibleFragments(baseline,
		[]schema.IdentifiedFragment{tenantB, tenantA, operator})

	assert.Empty(t, rejectedKeysOf(rejected), "nothing here dangles: %v", rejected)
	assert.Equal(t, []string{"op-ns/zzz-late-key", "tenant-ns/aaa", "tenant-ns/bbb"}, keysOf(accepted),
		"order must survive the post-pass: operator tier first, then Key within the tier")

	assertAcceptedResolves(t, baseline, accepted)
}

// The invariant asserted over the scenarios that already had a partition test,
// so a future edit to the walk cannot quietly break the guarantee while every
// named assertion still passes. This is the property; the tests above are the
// cases.
func TestPartition_AcceptedSetAlwaysResolves(t *testing.T) {
	baseline := []*spiceboxv1alpha1.SpiceDBSchemaFragment{
		rawZedFrag("definition basetype {\n\trelation member: user\n}\n"),
	}
	cases := []struct {
		name       string
		baseline   []*spiceboxv1alpha1.SpiceDBSchemaFragment
		candidates []schema.IdentifiedFragment
	}{
		{
			name: "a provider removed for its own reasons takes its consumer with it",
			candidates: []schema.IdentifiedFragment{
				{Key: "ns/a", Fragment: rawZedFrag("definition c_one {\n\trelation mem: p_one#member\n}\n")},
				{Key: "ns/z", Fragment: rawZedFrag(
					"definition p_one {\n\trelation member: user\n}\n" +
						"definition p_bad {\n\trelation bad: nope#member\n}\n")},
			},
		},
		{
			name:     "a dangling arrow onto a baseline type",
			baseline: baseline,
			candidates: []schema.IdentifiedFragment{
				{Key: "ns/evil", Fragment: rawZedFrag("definition e_res {\n\trelation src: basetype\n\tpermission viewx = src->nope\n}\n")},
				{Key: "ns/ok", Fragment: resourceFragment("t_widget", "reader", "read")},
			},
		},
		{
			name:     "colliding candidates, which defeat the all-candidates compose",
			baseline: baseline,
			candidates: []schema.IdentifiedFragment{
				{Key: "ns/a", Fragment: resourceFragment("shared_res", "reader", "read")},
				{Key: "ns/b", Fragment: resourceFragment("shared_res", "writer", "write")},
				{Key: "ns/c", Fragment: rawZedFrag("definition c_res {\n\trelation mem: nope#member\n}\n")},
			},
		},
		{
			name:     "a candidate colliding with the baseline",
			baseline: baseline,
			candidates: []schema.IdentifiedFragment{
				{Key: "ns/evil", Fragment: rawZedFrag("definition basetype {\n\trelation other: user\n}\n")},
				{Key: "ns/needs-it", Fragment: rawZedFrag("definition n_res {\n\trelation mem: basetype#member\n}\n")},
			},
		},
		{
			name:       "nothing at all",
			baseline:   baseline,
			candidates: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			accepted, _ := schema.PartitionCompatibleFragments(tc.baseline, tc.candidates)
			assertAcceptedResolves(t, tc.baseline, accepted)
		})
	}
}
