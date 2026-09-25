package precondition_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/precondition"
)

func obs(kv map[string]any) precondition.Facts {
	return precondition.Facts{Observed: kv, Envelope: map[string]any{}}
}

func TestEvaluateTriState(t *testing.T) {
	const expr = `facts.observed.is_cross_repository == false`

	cases := []struct {
		name        string
		facts       precondition.Facts
		want        precondition.Verdict
		wantMissing []precondition.FactRef
	}{
		{
			name:  "fact present and predicate true: satisfied",
			facts: obs(map[string]any{"is_cross_repository": false}),
			want:  precondition.Satisfied,
		},
		{
			name:  "fact present and predicate false: refused",
			facts: obs(map[string]any{"is_cross_repository": true}),
			want:  precondition.Refused,
		},
		{
			name:        "fact absent: UNDETERMINED, never refused — nobody has answered yet",
			facts:       obs(map[string]any{}),
			want:        precondition.Undetermined,
			wantMissing: []precondition.FactRef{{Provenance: "observed", Name: "is_cross_repository"}},
		},
		{
			name: "fact present holding nil: DETERMINED — a null the tool reported is evidence, " +
				"not absence; the test is key presence",
			facts: obs(map[string]any{"is_cross_repository": nil}),
			want:  precondition.Refused, // nil != false
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := precondition.Compile(expr)
			require.NoError(t, err)

			got, missing, err := precondition.Evaluate(c, tc.facts, "git_commit", "ba03f5969a")
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.ElementsMatch(t, tc.wantMissing, missing)
		})
	}
}

// TestEvaluateConjunctionStaysUndeterminedUntilEveryReferenceIsPresent is the
// monotonicity test. `a && b` with a=true and b unknown must NOT short-circuit
// to a decision: it can still only close further, never open.
func TestEvaluateConjunctionStaysUndeterminedUntilEveryReferenceIsPresent(t *testing.T) {
	c, err := precondition.Compile(`facts.observed.a && facts.observed.b`)
	require.NoError(t, err)

	got, missing, err := precondition.Evaluate(c, obs(map[string]any{"a": true}), "t", "i")
	require.NoError(t, err)
	assert.Equal(t, precondition.Undetermined, got)
	assert.ElementsMatch(t, []precondition.FactRef{{Provenance: "observed", Name: "b"}}, missing)

	// And a FALSE first operand is likewise undetermined, not refused: CEL would
	// short-circuit, but the design's verdict is over the whole predicate.
	got, _, err = precondition.Evaluate(c, obs(map[string]any{"a": false}), "t", "i")
	require.NoError(t, err)
	assert.Equal(t, precondition.Undetermined, got)
}

func TestEvaluateBindsSlot(t *testing.T) {
	c, err := precondition.Compile(`facts.observed.repo == slot.resourceID`)
	require.NoError(t, err)

	got, _, err := precondition.Evaluate(c,
		obs(map[string]any{"repo": "demo-org/demo-repo"}), "git_repo", "demo-org/demo-repo")
	require.NoError(t, err)
	assert.Equal(t, precondition.Satisfied, got)
}

// TestEvaluateSurfacesAnExpressionErrorRatherThanCallingItUndetermined is the
// reason references are extracted statically. A broken expression must be an
// ERROR the author can see, never a quiet "not yet known" that denies forever.
func TestEvaluateSurfacesAnExpressionErrorRatherThanCallingItUndetermined(t *testing.T) {
	c, err := precondition.Compile(`facts.observed.n > 0`)
	require.NoError(t, err)

	_, _, err = precondition.Evaluate(c, obs(map[string]any{"n": "not a number"}), "t", "i")
	require.Error(t, err, "a type error at eval must surface, not read as undetermined")
}

// TestEvaluateShortCircuitShapesStayUndetermined is the sibling of the
// conjunction test above, and the `||` row is the one that matters most.
//
// cel-go decides these shapes without evaluating every term: evalAnd returns
// False the moment any term is False, evalOr returns True the moment any term
// is True, and a conditional evaluates only the taken branch. So an evaluator
// that ran the program before checking presence would answer the rows below
// that carry a DECIDED operand from an input the recorded facts cannot decide.
//
// That qualifier is load-bearing, so do not generalize it away. A row whose
// operands are ALL absent cannot expose the ordering bug at all: with nothing
// concrete to short-circuit on, the select itself errors ("no such key") and
// even a broken evaluator falls through to the presence check. The
// both-provenances-absent row below is exactly that — real coverage for the
// missing-list and its sort order, and NOT proof of the ordering property.
// Each row's name states the operand it decides on for this reason; a future
// reader who deletes the decided-operand rows believing the last one covers
// them would be left with a table that cannot fail.
//
// The two directions are NOT equally bad, which is why both are pinned. `&&`
// with a false operand short-circuits to Refused — wrong, but a slot that
// stays shut. `||` with a TRUE operand short-circuits to SATISFIED, which
// BINDS a slot on a predicate no observation has cleared: the bypass §5 names,
// and strictly worse than the refusal. A suite that guarded only the `&&` row
// would catch the harmless regression and miss the dangerous one.
//
// missing is compared with Equal, not ElementsMatch: the order is the sorted
// order References promises, and a hold reason recorded from it must be stable
// across re-evaluations rather than churning with map order.
func TestEvaluateShortCircuitShapesStayUndetermined(t *testing.T) {
	cases := []struct {
		name        string
		expr        string
		facts       precondition.Facts
		wantMissing []precondition.FactRef
	}{
		{
			name:        "|| with a TRUE operand: the BYPASS direction — CEL alone would answer Satisfied and BIND",
			expr:        `facts.observed.a || facts.observed.b`,
			facts:       obs(map[string]any{"a": true}),
			wantMissing: []precondition.FactRef{{Provenance: "observed", Name: "b"}},
		},
		{
			name:        "&& with a FALSE operand: CEL alone would answer Refused",
			expr:        `facts.observed.a && facts.observed.b`,
			facts:       obs(map[string]any{"a": false}),
			wantMissing: []precondition.FactRef{{Provenance: "observed", Name: "b"}},
		},
		{
			name:        "ternary: the untaken branch is never evaluated, but the facts it reads still gate",
			expr:        `facts.observed.a ? facts.observed.b == 1 : facts.observed.c == 1`,
			facts:       obs(map[string]any{"a": true, "b": 1}),
			wantMissing: []precondition.FactRef{{Provenance: "observed", Name: "c"}},
		},
		{
			name:  "missing from BOTH provenances: every reference is reported, envelope sorted first",
			expr:  `facts.envelope.head_is_fork == false && facts.observed.is_cross_repository == false`,
			facts: precondition.Facts{}, // nil maps: nothing recorded of either provenance
			wantMissing: []precondition.FactRef{
				{Provenance: "envelope", Name: "head_is_fork"},
				{Provenance: "observed", Name: "is_cross_repository"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := precondition.Compile(tc.expr)
			require.NoError(t, err)

			got, missing, err := precondition.Evaluate(c, tc.facts, "t", "i")
			require.NoError(t, err)
			assert.Equal(t, precondition.Undetermined, got)
			assert.Equal(t, tc.wantMissing, missing)
		})
	}
}

// TestEvaluateResolvesEachReferenceAgainstItsOwnProvenance pins that an
// envelope reference is answered from Facts.Envelope and never from
// Facts.Observed. Provenance is a trust grade, not a namespace: a fact the
// platform derived from a signed envelope is a stronger claim than one derived
// from a response to a call the agent shaped, so answering an envelope-gated
// predicate out of the observed map would silently hand the author weaker
// evidence than the one they demanded — and would do it while looking green.
func TestEvaluateResolvesEachReferenceAgainstItsOwnProvenance(t *testing.T) {
	c, err := precondition.Compile(
		`facts.envelope.head_is_fork == false && facts.observed.is_cross_repository == false`)
	require.NoError(t, err)

	got, missing, err := precondition.Evaluate(c, precondition.Facts{
		Envelope: map[string]any{"head_is_fork": false},
		Observed: map[string]any{"is_cross_repository": false},
	}, "github_pr", "demo-org/demo-repo#6")
	require.NoError(t, err)
	assert.Equal(t, precondition.Satisfied, got)
	assert.Empty(t, missing)

	// The same two names, both recorded — but both as OBSERVED. The envelope
	// reference is still missing, so the verdict is undetermined rather than
	// satisfied by the lower-graded copy.
	got, missing, err = precondition.Evaluate(c, precondition.Facts{
		Observed: map[string]any{"head_is_fork": false, "is_cross_repository": false},
	}, "github_pr", "demo-org/demo-repo#6")
	require.NoError(t, err)
	assert.Equal(t, precondition.Undetermined, got)
	assert.Equal(t,
		[]precondition.FactRef{{Provenance: "envelope", Name: "head_is_fork"}}, missing)
}

func TestVerdictString(t *testing.T) {
	assert.Equal(t, "undetermined", precondition.Undetermined.String())
	assert.Equal(t, "satisfied", precondition.Satisfied.String())
	assert.Equal(t, "refused", precondition.Refused.String())
	// An unknown verdict must never render as one of the three: a bad cast
	// reading as "satisfied" in a log is how an operator concludes the gate
	// opened legitimately.
	assert.Equal(t, "Verdict(7)", precondition.Verdict(7).String())
}
