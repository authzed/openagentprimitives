package schema

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// equivalenceFixtures is shared by both tests in this file — define it once,
// at package level, not inside a test function.
func equivalenceFixtures() []IdentifiedFragment {
	return []IdentifiedFragment{
		{
			Key:  "mcpserver/default/alpha",
			Tier: TierTenant,
			Fragment: &spiceboxv1alpha1.SpiceDBSchemaFragment{
				Resources: []spiceboxv1alpha1.SpiceDBResource{{
					Name:        "alpha_widget",
					Standing:    spiceboxv1alpha1.StandingSessionOnly,
					Relations:   []spiceboxv1alpha1.SpiceDBRelation{{Name: "owner", SubjectType: "user"}},
					Permissions: []spiceboxv1alpha1.SpiceDBPermission{{Name: "read", Expr: "owner"}},
				}},
			},
		},
		{
			Key:  "mcpserver/default/beta",
			Tier: TierTenant,
			Fragment: &spiceboxv1alpha1.SpiceDBSchemaFragment{
				// Byte-identical to alpha's resource. Today this DEDUPES, and
				// that is the behaviour the next test pins.
				Resources: []spiceboxv1alpha1.SpiceDBResource{{
					Name:        "alpha_widget",
					Standing:    spiceboxv1alpha1.StandingSessionOnly,
					Relations:   []spiceboxv1alpha1.SpiceDBRelation{{Name: "owner", SubjectType: "user"}},
					Permissions: []spiceboxv1alpha1.SpiceDBPermission{{Name: "read", Expr: "owner"}},
				}},
				RawZed: "definition beta_crate {\n\trelation holder: user\n\tpermission open = holder\n}\n",
			},
		},
		{
			Key:  "spicedbbootstrap/default/gamma",
			Tier: TierOperator,
			Fragment: &spiceboxv1alpha1.SpiceDBSchemaFragment{
				RawZed: "definition gamma_shelf {\n\trelation keeper: user\n\tpermission browse = keeper\n}\n",
			},
		},
	}
}

// equivalenceBaseline is the "channelKindFragments" / baseline parameter
// composeAllParseOnly and composeAllWithSkipped each take SEPARATELY from
// the CR-sourced mcpFragments slice — production always supplies a non-empty
// one (the registered channel kinds' fragments UNIONED with the built-in
// toolkit fragments; see composeAllWithSkipped's own doc). A nil baseline
// would leave that whole parameter, and the fixed-position ordering it
// controls, unexercised by the equivalence guard below.
func equivalenceBaseline() []*spiceboxv1alpha1.SpiceDBSchemaFragment {
	return []*spiceboxv1alpha1.SpiceDBSchemaFragment{
		{RawZed: "definition delta_baseline {\n\trelation owner: user\n\tpermission read = owner\n}\n"},
	}
}

// TestComposeFragments_MatchesTheConcatenationItReplaces is a standing
// anti-drift guard, not a one-time migration proof. It compares
// composeAllWithSkipped (the validating assembly RunAll actually writes
// through) against composeAllParseOnly — the PARSE-ONLY assembly that still
// actually runs, today, at all 7 partition/ValidateFragment call sites — not
// against a hand-reconstructed inline stand-in. If composeAllParseOnly ever
// drifts from composeAllWithSkipped's SCHEMA (not byte-for-byte: the
// generator normalises comments, whitespace and definition order, which is
// the point, so both sides are compared through canonicalizeSchema, the same
// canonical form RunAll already uses to decide whether a write is needed),
// the partition would start judging candidates against a schema RunAll does
// not actually write — and comparing against a throwaway inline
// reconstruction, as this test originally did, would stay green through that
// drift instead of catching it.
//
// Both mcpFragments AND a non-empty baseline are passed to BOTH assembly
// functions (composeAllWithSkipped rather than composeFragmentSet directly,
// which only takes one flat slice and so cannot exercise the two-parameter
// merge at all). composeAllParseOnly flattens mcpFragments before
// channelKindFragments; composeAllWithSkipped flattens channelKindFragments
// (the baseline) before mcpFragments — see composeAllWithSkipped's own doc
// for why the baseline goes first there. If that difference in merge ORDER
// ever produced a difference in composed MEANING (rather than being fully
// absorbed by canonicalizeSchema's own definition-order normalisation), this
// is the test that would catch it; with only one baseline instance it can
// never collide, so it never had to name a name.
//
// With nil pairs and no pre-existing grant relations in the scaffold,
// composeAllParseOnly's own ComposeWithSkipped step early-returns its input
// unchanged, so this is not a behavior change from the bare concatenation
// the test originally compared against — only a change in WHAT it guards.
func TestComposeFragments_MatchesTheConcatenationItReplaces(t *testing.T) {
	frags := equivalenceFixtures()
	baseline := equivalenceBaseline()

	oldText, err := composeAllParseOnly(frags, baseline, nil)
	require.NoError(t, err)

	newText, skipped, err := composeAllWithSkipped(frags, baseline, nil, nil)
	require.NoError(t, err)
	require.Empty(t, skipped, "no grant pairs were passed, so nothing should be skipped")

	canonOld, err := canonicalizeSchema(oldText)
	require.NoError(t, err, "the parse-only path's output must itself be valid")
	canonNew, err := canonicalizeSchema(newText)
	require.NoError(t, err)
	assert.Equal(t, canonOld, canonNew,
		"composing through the compiler must yield the same schema as the parse-only path callers still use")

	// Both RawZed contributors (beta's and gamma's) must survive assembly,
	// not just the structured/deduped half canonOld/canonNew already cover
	// indirectly — a per-contributor RawZed NamedFragment silently dropped
	// would still leave the byte counts canonicalizeSchema compares plausible
	// if the dropped definition happened to be small, so name them directly.
	assert.Contains(t, newText, "definition beta_crate")
	assert.Contains(t, newText, "definition gamma_shelf")
	// The baseline parameter itself must actually reach the composed text —
	// without this, a regression that dropped channelKindFragments entirely
	// could still leave the canonical-equivalence assertion above passing.
	assert.Contains(t, newText, "definition delta_baseline",
		"the baseline (channelKindFragments) parameter must reach the composed text")
}

// TestComposeAll_FragmentKeyAndTierAreIdentityMetadataOnly is the
// behaviour-neutrality guard for threading fragment identity through the
// compose path: ComposeAll/RunAll take []IdentifiedFragment instead of a
// bare []*SpiceDBSchemaFragment so a later task can name the offending
// contributor in an error. Key IS read on this path — composeFragmentSet
// names a RawZed fragment's synthetic file by it (see that function's doc) —
// which is exactly what lets Key decide definition ORDER in the composed
// TEXT once two or more RawZed fragments are in play. What Key/Tier must
// never do is change what the schema MEANS.
//
// A single-fragment fixture cannot prove that: with one RawZed contributor
// there is nothing else to sort against, so a byte-identical assertion holds
// whether or not Key is neutral — a prior version of this test was exactly
// that, and passed for the wrong reason (see the comment left in its place
// in composer_test.go). Two RawZed fragments, reordered by giving one a real
// Key that alphabetically sorts ahead of the other's zero-value "baseline"
// name, are what actually exercise it: the raw TEXT differs (definition
// order flips), and canonicalizeSchema — the same canonical form RunAll's
// own write-if-changed comparison uses — is what proves the difference is
// exactly that ordering and nothing else.
func TestComposeAll_FragmentKeyAndTierAreIdentityMetadataOnly(t *testing.T) {
	fragA := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		RawZed: "definition alpha_res {\n\trelation owner: user\n\tpermission view = owner\n}\n",
	}
	fragB := &spiceboxv1alpha1.SpiceDBSchemaFragment{
		RawZed: "definition zulu_res {\n\trelation owner: user\n\tpermission view = owner\n}\n",
	}
	pairs := []GrantPair{
		{ResourceType: "alpha_res", Permission: "view"},
		{ResourceType: "zulu_res", Permission: "view"},
	}

	// unkeyed: both fragments carry the zero-value Key, so composeFragmentSet
	// names them by their position in the slice — "200-baseline-00" (fragA)
	// then "200-baseline-01" (fragB) — which sort in that same order.
	unkeyed, err := ComposeAll([]IdentifiedFragment{
		{Fragment: fragA}, {Fragment: fragB},
	}, nil, pairs)
	require.NoError(t, err, "ComposeAll with zero-value Key/Tier")

	// keyed: fragB's real Key ("001-fragB", named "100-001-fragB" once
	// composeFragmentSet's "100-" prefix is applied) sorts alphabetically
	// BEFORE fragA's implicit "200-baseline-00", flipping which fragment's
	// definition the compiler emits first.
	keyed, err := ComposeAll([]IdentifiedFragment{
		{Fragment: fragA},
		{Key: "001-fragB", Tier: TierOperator, Fragment: fragB},
	}, nil, pairs)
	require.NoError(t, err, "ComposeAll with a real Key/Tier")

	// Anchor: without this, a regression that dropped mcpFragments entirely
	// would leave both sides as the bare scaffold and every assertion below
	// would still hold, proving nothing about Key/Tier specifically.
	require.Contains(t, unkeyed, "definition alpha_res {")
	require.Contains(t, unkeyed, "definition zulu_res {")

	assert.NotEqual(t, unkeyed, keyed,
		"a real Key must change composeFragmentSet's synthetic filename and so the raw definition order — "+
			"if this now holds, the fixture stopped exercising what this test guards")

	canonUnkeyed, err := canonicalizeSchema(unkeyed)
	require.NoError(t, err)
	canonKeyed, err := canonicalizeSchema(keyed)
	require.NoError(t, err)
	assert.Equal(t, canonUnkeyed, canonKeyed,
		"Key and Tier are identity metadata for reporting only — they may reorder the composed TEXT "+
			"but must never change the composed SCHEMA's canonical meaning")
}

// Byte-identical structured resources across two fragments dedupe today. A
// per-fragment emission would make them a hard collision instead.
func TestComposeFragmentSet_DedupesIdenticalStructuredResources(t *testing.T) {
	// alpha and beta declare a byte-identical `alpha_widget`. Reuse them.
	out, err := composeFragmentSet(equivalenceFixtures()[:2])
	require.NoError(t, err,
		"byte-identical structured resources dedupe; they are not a collision")
	assert.Equal(t, 1, strings.Count(out, "definition alpha_widget"),
		"the deduped resource must appear exactly once in the composed schema")
}

// TestComposeFragmentSet_HandlesRealContributorKeyShapes guards the one thing
// composeFragmentSet is the FIRST code to do: turn a contributor Key into a
// filename in ComposeFragments' synthetic filesystem (see its own "must be a
// single path segment" check). A key that check refuses fails the WHOLE
// compose — not a per-contributor rejection the partition could isolate,
// since composeFragmentSet runs before any partition step — so a rejected
// real key shape would be a cluster-wide write freeze.
//
// Table-driven over the four shapes agentsessiongrants_controller.go
// actually constructs (MCPServer, SidecarToolbox, SpiceboxToolkit,
// SpiceDBBootstrap), not a synthetic key nobody would ever pass in: three of
// the four embed a ":" (SidecarToolbox/SpiceboxToolkit/SpiceDBBootstrap all
// prefix with "<kind>:"), which composeFragmentSet's "/" → "_" replacement
// does nothing about.
func TestComposeFragmentSet_HandlesRealContributorKeyShapes(t *testing.T) {
	cases := []struct {
		name string
		key  string
	}{
		{name: "MCPServer: namespace/name", key: "default/github"},
		{name: "SidecarToolbox: kind-prefixed namespace/name", key: "sidecartoolbox:default/toolbox"},
		{name: "SpiceboxToolkit: kind-prefixed name, no namespace", key: "spiceboxtoolkit:kit"},
		{name: "SpiceDBBootstrap: kind-prefixed namespace/name", key: "spicedbbootstrap:default/boot"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frag := IdentifiedFragment{
				Key:  tc.key,
				Tier: TierTenant,
				Fragment: &spiceboxv1alpha1.SpiceDBSchemaFragment{
					RawZed: "definition key_shape_probe {\n\trelation owner: user\n\tpermission read = owner\n}\n",
				},
			}
			out, err := composeFragmentSet([]IdentifiedFragment{frag})
			require.NoError(t, err, "a real contributor key shape must not be refused by the synthetic filesystem")
			assert.Contains(t, out, "definition key_shape_probe")
		})
	}
}
