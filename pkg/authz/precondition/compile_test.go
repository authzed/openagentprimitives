package precondition_test

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/precondition"
)

func TestCompileExtractsFactReferences(t *testing.T) {
	cases := []struct {
		name string
		expr string
		want []precondition.FactRef
	}{
		{
			name: "one observed fact",
			expr: `facts.observed.is_cross_repository == false`,
			want: []precondition.FactRef{{Provenance: "observed", Name: "is_cross_repository"}},
		},
		{
			name: "one envelope fact",
			expr: `facts.envelope.head_is_fork == false`,
			want: []precondition.FactRef{{Provenance: "envelope", Name: "head_is_fork"}},
		},
		{
			name: "a conjunction reads BOTH, which is why undetermined must consider every reference",
			expr: `facts.envelope.head_is_fork == false && facts.observed.is_cross_repository == false`,
			want: []precondition.FactRef{
				{Provenance: "envelope", Name: "head_is_fork"},
				{Provenance: "observed", Name: "is_cross_repository"},
			},
		},
		{
			name: "the same fact twice is one reference",
			expr: `facts.observed.n > 0 && facts.observed.n < 10`,
			want: []precondition.FactRef{{Provenance: "observed", Name: "n"}},
		},
		{
			// The brief's row used `facts.observed.a ? facts.observed.b : false`,
			// which type-checks to dyn and is refused by the bool rule below.
			// The claim under test is the walk reaching a ternary branch, so the
			// branches are made bool and the claim is unchanged.
			name: "a reference inside a ternary still counts — it may be read at runtime",
			expr: `facts.observed.a ? facts.observed.b == 1 : false`,
			want: []precondition.FactRef{
				{Provenance: "observed", Name: "a"},
				{Provenance: "observed", Name: "b"},
			},
		},
		{
			name: "slot is addressable and is not a fact reference",
			expr: `facts.observed.repo == slot.resourceID`,
			want: []precondition.FactRef{{Provenance: "observed", Name: "repo"}},
		},
		{
			// A macro expands into a comprehension, so a reference in the
			// predicate body is nowhere near a plain binary operand. Missing it
			// would leave that fact unchecked for presence, which is the one
			// failure mode of this package that opens the gate.
			name: "a reference inside .exists() counts, target and predicate body alike",
			expr: `facts.observed.commits.exists(c, c.sha == facts.envelope.head_sha)`,
			want: []precondition.FactRef{
				{Provenance: "observed", Name: "commits"},
				{Provenance: "envelope", Name: "head_sha"},
			},
		},
		{
			name: "a reference inside .all() over a list literal counts, in the iteration range and the body",
			expr: `[facts.observed.a, facts.envelope.b].all(x, x == facts.observed.c)`,
			want: []precondition.FactRef{
				{Provenance: "observed", Name: "a"},
				{Provenance: "envelope", Name: "b"},
				{Provenance: "observed", Name: "c"},
			},
		},
		{
			name: "a reference inside a nested .filter().map() comprehension counts",
			expr: `facts.observed.items.filter(i, i.kind == facts.envelope.kind).map(i, i.n).exists(n, n > facts.observed.floor)`,
			want: []precondition.FactRef{
				{Provenance: "observed", Name: "items"},
				{Provenance: "envelope", Name: "kind"},
				{Provenance: "observed", Name: "floor"},
			},
		},
		{
			name: "a reference inside a function-call argument and a map value counts",
			expr: `size({"k": facts.observed.list}["k"]) > size(facts.envelope.list)`,
			want: []precondition.FactRef{
				{Provenance: "observed", Name: "list"},
				{Provenance: "envelope", Name: "list"},
			},
		},
		{
			// Deeper selects read INTO a fact's value; the fact itself is still
			// the thing whose presence decides undetermined.
			name: "a nested select names the fact at its root, not the leaf",
			expr: `facts.observed.pr.head.repo == "x"`,
			want: []precondition.FactRef{{Provenance: "observed", Name: "pr"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := precondition.Compile(tc.expr)
			require.NoError(t, err)
			assert.ElementsMatch(t, tc.want, c.References())
			assert.NotNil(t, c.Program(), "a compiled precondition must carry a runnable program")
			assert.Equal(t, tc.expr, c.Expression())
		})
	}
}

func TestCompileRejects(t *testing.T) {
	cases := []struct {
		name    string
		expr    string
		wantErr string
	}{
		{
			name:    "a non-bool expression: a precondition must decide",
			expr:    `facts.observed.name`,
			wantErr: "must return bool",
		},
		{
			// The brief's ternary row: dyn is refused here even though the same
			// shape with bool branches compiles, because a precondition that
			// might not be a bool is one that might not decide.
			name:    "a ternary whose branches type-check to dyn: still not a decision",
			expr:    `facts.observed.a ? facts.observed.b : false`,
			wantErr: "must return bool",
		},
		{
			name:    "an unknown top-level binding",
			expr:    `nonsense.field == 1`,
			wantErr: "undeclared reference",
		},
		{
			name:    "an unknown provenance: only envelope and observed exist",
			expr:    `facts.guessed.x == 1`,
			wantErr: "unknown fact provenance",
		},
		{
			name: "has() over facts turns 'not yet known' into a decidable value, " +
				"which is the tri-state collapse the design forbids",
			expr:    `has(facts.observed.x) && facts.observed.x`,
			wantErr: "has() is not available over facts",
		},
		{
			name:    "has() over a whole provenance namespace probes presence just as directly",
			expr:    `has(facts.observed)`,
			wantErr: "has() is not available over facts",
		},
		{
			name:    "has() inside a macro body is still has() over facts",
			expr:    `facts.observed.items.exists(i, has(facts.envelope.q))`,
			wantErr: "has() is not available over facts",
		},
		{
			name:    "syntax error",
			expr:    `facts.observed.x ==`,
			wantErr: "compile",
		},
		{
			// Index syntax reads a fact the walk cannot name — and a fact it
			// cannot name is a fact it cannot check for presence.
			name:    "index syntax over facts is not a nameable reference",
			expr:    `facts["observed"]["x"] == 1`,
			wantErr: "facts must be addressed as facts.<provenance>.<name>",
		},
		{
			name:    "a whole-namespace read is not a nameable reference",
			expr:    `size(facts.observed) > 0`,
			wantErr: "facts must be addressed as facts.<provenance>.<name>",
		},
		{
			name:    "facts passed whole to a function is not a nameable reference",
			expr:    `size(facts) > 0`,
			wantErr: "facts must be addressed as facts.<provenance>.<name>",
		},
		{
			// `in` over a namespace is a presence probe wearing different
			// punctuation: it answers "has this fact been recorded" as a bool,
			// which is the tri-state collapse has() is refused for.
			name:    "membership over a whole provenance namespace probes presence",
			expr:    `"x" in facts.observed`,
			wantErr: "facts must be addressed as facts.<provenance>.<name>",
		},
		{
			// So does iterating the namespace's keys — a comprehension over
			// facts.observed reads which facts exist rather than what one says.
			name:    "a comprehension over a whole provenance namespace probes presence",
			expr:    `facts.observed.exists(k, k == "is_cross_repository")`,
			wantErr: "facts must be addressed as facts.<provenance>.<name>",
		},
		{
			// The "flows through a function, then gets selected from" shape: the
			// select is no longer rooted at the facts identifier, so no reference
			// is nameable and the read must be refused rather than skipped.
			name:    "a fact reached through a function call is not rooted at the facts binding",
			expr:    `dyn(facts).observed.x == 1`,
			wantErr: "facts must be addressed as facts.<provenance>.<name>",
		},
		{
			// Shadowing would make the walk and the running expression disagree
			// about what `facts.observed.x` denotes.
			name:    "a comprehension variable may not shadow the facts binding",
			expr:    `[{"observed": {"x": true}}].all(facts, facts.observed.x)`,
			wantErr: "shadows the facts binding",
		},
		{
			name:    "a comprehension variable may not shadow the slot binding",
			expr:    `[1].all(slot, slot > 0)`,
			wantErr: "shadows the slot binding",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := precondition.Compile(tc.expr)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestCompileAllowsProbingInsideAPresentFact pins the boundary of the has()
// refusal: probing a sub-field of a fact is fine, because the FACT is still
// presence-gated and an absent one is undetermined regardless of what the
// expression does with it. Only probing a fact NAME collapses the tri-state.
func TestCompileAllowsProbingInsideAPresentFact(t *testing.T) {
	c, err := precondition.Compile(`has(facts.observed.pr.head) && facts.observed.pr.head.fork == false`)
	require.NoError(t, err)
	assert.Equal(t, []precondition.FactRef{{Provenance: "observed", Name: "pr"}}, c.References())
}

// TestReferencesAreDeterministicAndOwnedByTheCaller guards two properties the
// downstream gate leans on: status.factSources must not churn between
// reconciles, and a caller must not be able to shrink the set that decides
// undetermined.
func TestReferencesAreDeterministicAndOwnedByTheCaller(t *testing.T) {
	const expr = `facts.observed.z == 1 && facts.envelope.a == 2 && facts.observed.b == 3`
	c, err := precondition.Compile(expr)
	require.NoError(t, err)

	want := []precondition.FactRef{
		{Provenance: "envelope", Name: "a"},
		{Provenance: "observed", Name: "b"},
		{Provenance: "observed", Name: "z"},
	}
	assert.Equal(t, want, c.References(), "references are sorted by provenance then name")

	got := c.References()
	got[0] = precondition.FactRef{Provenance: "observed", Name: "tampered"}
	assert.Equal(t, want, c.References(), "References returns a copy; a caller cannot drop a reference")
}

// factAddressInText finds every `facts.<provenance>.<name>` written in the
// expression's SOURCE TEXT. A deeper chain contributes only its root fact,
// which is the fact whose presence decides undetermined.
var factAddressInText = regexp.MustCompile(`facts\.([A-Za-z_][A-Za-z0-9_]*)\.([A-Za-z_][A-Za-z0-9_]*)`)

// TestEveryFactWrittenIsAFactExtracted cross-checks the AST walk against the
// expression text, which is derived a completely different way and so cannot
// share the walk's blind spots.
//
// This is the test that guards the one failure mode of this package that is a
// security defect rather than an inconvenience. The reference set decides
// undetermined; a reference the walk misses is a fact nobody checks for
// presence, which turns undetermined into satisfied and opens the gate. The
// corpus is therefore deliberately awkward — macros nested in macros, a
// reference in an accumulator body, in a ternary branch, in a map value, in a
// function argument, in an index expression, in a comparison against `slot` —
// so that a walk which only reached plain binary operands would fail here even
// though every table row above still passed.
func TestEveryFactWrittenIsAFactExtracted(t *testing.T) {
	corpus := []string{
		`facts.observed.a == 1`,
		`facts.envelope.a == 1 && facts.observed.b == 2 || facts.envelope.c == 3`,
		`!(facts.observed.a == 1)`,
		`facts.observed.a == 1 ? facts.envelope.b == 2 : facts.observed.c == 3`,
		`facts.observed.list.exists(i, i == facts.envelope.needle)`,
		`facts.observed.list.exists_one(i, i.k == facts.envelope.k)`,
		`facts.observed.list.all(i, i.n > facts.envelope.floor)`,
		`facts.observed.list.filter(i, i.k == facts.envelope.k).size() > 0`,
		`facts.observed.list.map(i, i.n + facts.envelope.offset).exists(n, n > 0)`,
		`facts.observed.outer.exists(i, facts.envelope.inner.exists(j, j == i && facts.observed.pivot == j))`,
		`size([facts.observed.a, facts.envelope.b]) == size(facts.observed.c)`,
		`{"x": facts.observed.a, facts.envelope.k: 2}["x"] == facts.observed.b`,
		`facts.observed.a in [facts.envelope.b, facts.observed.c]`,
		`facts.observed.repo == slot.resourceID && facts.envelope.kind == slot.resourceType`,
		`facts.observed.pr.head.repo.owner == facts.envelope.base.repo.owner`,
		`[facts.observed.a].all(x, [facts.envelope.b].exists(y, x == y))`,
		`string(facts.observed.n).startsWith(string(facts.envelope.prefix))`,
		`facts.observed.list[facts.envelope.idx] == facts.observed.want`,
		`has(facts.observed.pr.head) && facts.envelope.ok == true`,
		`timestamp(facts.observed.at) > timestamp(facts.envelope.cutoff)`,
	}
	for _, expr := range corpus {
		t.Run(expr, func(t *testing.T) {
			c, err := precondition.Compile(expr)
			require.NoError(t, err, "corpus expressions are all valid preconditions")

			want := map[precondition.FactRef]struct{}{}
			for _, m := range factAddressInText.FindAllStringSubmatch(expr, -1) {
				want[precondition.FactRef{Provenance: m[1], Name: m[2]}] = struct{}{}
			}
			require.NotEmpty(t, want, "the corpus row must name at least one fact")

			got := map[precondition.FactRef]struct{}{}
			for _, ref := range c.References() {
				got[ref] = struct{}{}
			}
			for ref := range want {
				assert.Contains(t, got, ref, "a fact written in the expression was not extracted, so nothing would check it for presence")
			}
		})
	}
}
