package observe_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/observe"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/factcontent"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/observedfact"

	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
)

// These tests drive a fact through the WHOLE production path — CEL evaluation
// (observe.Evaluate) into a real append-only memory facade (factcontent.Record)
// and back out of it (factcontent.ForSubject) — rather than asserting on the
// converter in isolation.
//
// That span is the point, and it is what the converter's own unit test
// (relwrites.TestEvalAnyWithItem_ReturnsJSONShapedValues) cannot show. Two of
// the three things the fidelity bug broke happen AFTER the conversion returns:
// the structure is destroyed at the JSON boundary inside Record, and the
// write-once comparison that silently absorbed a contradicting composite lives
// in Record too, not in the converter. Asserting on the Go value alone would
// pin the shape and still say nothing about what a later gate reads back.

// factRoundTrip records ONE named fact about ONE subject and reads it back,
// exactly the way a tool result becomes an observed_fact in production.
//
// It returns the value as a later gate would actually see it: decoded from the
// stored JSON, so a composite that was stored empty comes back empty here
// rather than being papered over by comparing pre-storage Go values.
func factRoundTrip(t *testing.T, factExpr string, vars map[string]any) (any, error) {
	t.Helper()

	block := observe.Block{
		Subjects: []observe.SubjectExpr{
			{ResourceType: `"github_pr"`, ResourceID: `"demo-org/demo-repo#6"`},
		},
		Facts: map[string]string{"probe": factExpr},
	}

	obs, err := observe.Evaluate(block, vars)
	if err != nil {
		return nil, err
	}
	require.Len(t, obs, 1, "no forEach means exactly one observation")

	// A real facade, not a fake: the append-only door and the JSON encode /
	// decode round trip are the two things under test here, and a stand-in for
	// either would assert nothing.
	ctx := memory.WithSystemApproval(context.Background(), "test")
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "default/demo-session"}

	if err := factcontent.Record(ctx, m, scope, observedfact.KindName, observedfact.IDPrefix, obs[0]); err != nil {
		return nil, err
	}

	facts, err := factcontent.ForSubject(ctx, m, scope, observedfact.KindName, "github_pr", "demo-org/demo-repo#6")
	if err != nil {
		return nil, err
	}
	require.Contains(t, facts, "probe", "the fact must be readable back under its own name")
	return facts["probe"], nil
}

// prItems is the shape a list-returning tool result arrives in: JSON-decoded
// Go values, which is why a plain field reference to it is already native and
// a CEL macro over it is not.
func prItems() map[string]any {
	return map[string]any{"items": []any{
		map[string]any{"id": "a", "ok": true, "n": 1},
		map[string]any{"id": "b", "ok": false, "n": 2},
	}}
}

func TestFactValueRoundTripsWithItsStructureIntact(t *testing.T) {
	cases := []struct {
		name string
		expr string
		want string // the fact's value as JSON, exactly as a later gate would decode it
	}{
		{
			// THE bug. A list built by the `map` macro is []ref.Val, which
			// marshals to a list of empty objects: every id and flag gone,
			// with no error anywhere to notice it.
			name: "list of maps from the map macro: every element and key survives",
			expr: `result.items.map(i, {"id": i.id, "ok": i.ok})`,
			want: `[{"id":"a","ok":true},{"id":"b","ok":false}]`,
		},
		{
			// celconv.List's lesson, restated as a fact value: a macro-built list
			// and a plain field reference have DIFFERENT internal
			// representations, so a fix that handles one can still lose the
			// other. Both are covered deliberately.
			name: "list from the filter macro: the surviving element keeps its keys",
			expr: `result.items.filter(i, i.ok)`,
			want: `[{"id":"a","ok":true,"n":1}]`,
		},
		{
			name: "plain field-reference list: already native, still correct",
			expr: `result.items`,
			want: `[{"id":"a","ok":true,"n":1},{"id":"b","ok":false,"n":2}]`,
		},
		{
			// A CEL-built map's Value() is map[ref.Val]ref.Val, which does not
			// marshal at all — loud rather than silent, but still a fact that
			// could never be recorded.
			name: "bare map: keys and values survive",
			expr: `{"id": result.items[0].id, "count": size(result.items)}`,
			want: `{"id":"a","count":2}`,
		},
		{
			// The case a ONE-LEVEL conversion would still lose: the outer map
			// converts, and its list value stays in cel-go's internal form.
			name: "nested map > list > map: converted all the way down",
			expr: `{"outer": [{"inner": result.items[0].n}], "flat": "x"}`,
			want: `{"outer":[{"inner":1}],"flat":"x"}`,
		},
		{
			name: "nested list of lists: inner lists are converted too",
			expr: `[[1, 2], [3, size(result.items)]]`,
			want: `[[1,2],[3,2]]`,
		},
		{
			name: "list of maps each holding a list: three levels deep",
			expr: `result.items.map(i, {"id": i.id, "tags": [i.n, i.n + 1]})`,
			want: `[{"id":"a","tags":[1,2]},{"id":"b","tags":[2,3]}]`,
		},
		{
			name: "empty list from a macro: an empty list, not null",
			expr: `result.items.filter(i, i.n > 99)`,
			want: `[]`,
		},

		// Scalars are the pre-existing behaviour and must not regress. A fact
		// may legitimately be false, 0 or "" — see EvalAnyWithItem.
		{name: "scalar bool false: still false, not absent", expr: `result.items[1].ok`, want: `false`},
		{name: "scalar int: still an int", expr: `size(result.items)`, want: `2`},
		{name: "scalar string empty: still an empty string", expr: `""`, want: `""`},
		{name: "scalar string: unchanged", expr: `result.items[0].id`, want: `"a"`},
		{name: "scalar double: unchanged", expr: `1.5`, want: `1.5`},
		{
			// CEL null's Value() is a structpb.NullValue, an int32-backed
			// protobuf enum: stored raw it reads back as the NUMBER ZERO, which
			// a gate would read as a real value rather than as "no value".
			name: "null: stored as null, not as the number zero",
			expr: `null`,
			want: `null`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := factRoundTrip(t, tc.expr, map[string]any{"result": prItems()})
			require.NoError(t, err)

			gotJSON, err := json.Marshal(got)
			require.NoError(t, err, "a recorded fact must be re-encodable; %#v is not", got)
			assert.JSONEq(t, tc.want, string(gotJSON))
		})
	}
}

func TestFactValueWithNonStringMapKeysIsRefused(t *testing.T) {
	cases := []struct {
		name string
		expr string
	}{
		{name: "int keys", expr: `{1: "a", 2: "b"}`},
		{name: "uint keys", expr: `{1u: "a"}`},
		{name: "bool keys", expr: `{true: "a"}`},
		{name: "non-string key nested inside a list", expr: `[{1: "a"}]`},
		// Nested inside a map VALUE, not a list. The mechanism is the same
		// recursion the row above proves; this pins that the map branch descends
		// into its values too, which is the half a list-only case leaves open.
		{name: "non-string key nested inside a map value", expr: `{"a": {1: "b"}}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := factRoundTrip(t, tc.expr, map[string]any{"result": prItems()})

			// Failing closed is the requirement, and the message has to say
			// WHY: a fact nobody can decode must not be recorded as though it
			// were fine, and the author needs to know the key type is the
			// reason rather than hunting a storage-layer marshal error.
			require.Error(t, err)
			assert.Contains(t, err.Error(), "JSON object keys are strings")
			assert.Contains(t, err.Error(), `fact "probe"`, "the error must name the offending fact")
		})
	}
}

func TestForEachItemCompositeFactRoundTrips(t *testing.T) {
	// The forEach shape, which is how a real `observes` block reaches a
	// composite: the fact is built from the per-iteration `item` binding, not
	// from `result` directly. EvalAnyWithItem overlays `item` itself, so a
	// conversion that only handled the top-level bindings would pass every
	// test above and still lose this.
	obs, err := observe.Evaluate(observe.Block{
		ForEach: "result.items",
		Subjects: []observe.SubjectExpr{
			{ResourceType: `"github_pr"`, ResourceID: `"pr-" + item.id`},
		},
		Facts: map[string]string{"probe": `{"id": item.id, "tags": [item.n, item.n + 1]}`},
	}, map[string]any{"result": prItems()})
	require.NoError(t, err)
	require.Len(t, obs, 2, "two items yield two observations")

	ctx := memory.WithSystemApproval(context.Background(), "test")
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "default/demo-session"}
	for _, o := range obs {
		require.NoError(t, factcontent.Record(ctx, m, scope, observedfact.KindName, observedfact.IDPrefix, o))
	}

	for id, want := range map[string]string{
		"pr-a": `{"id":"a","tags":[1,2]}`,
		"pr-b": `{"id":"b","tags":[2,3]}`,
	} {
		facts, err := factcontent.ForSubject(ctx, m, scope, observedfact.KindName, "github_pr", id)
		require.NoError(t, err)
		require.Contains(t, facts, "probe")
		got, err := json.Marshal(facts["probe"])
		require.NoError(t, err)
		assert.JSONEq(t, want, string(got), "subject %s", id)
	}
}

func TestCompositeFactIsIdempotentButStillRefusesContradiction(t *testing.T) {
	// Write-once for composites is the property the fidelity bug quietly
	// broke: two DIFFERENT lists of maps both marshaled to a list of empty
	// objects, so factcontent.Record's canonical-JSON comparison judged them
	// equal and absorbed the second as a repeat observation. The contradiction
	// it exists to catch went unreported.
	ctx := memory.WithSystemApproval(context.Background(), "test")
	m := memory.NewLocal(inmem.NewBackend())
	scope := memory.Scope{Kind: "session", ID: "default/demo-session"}

	record := func(t *testing.T, expr string) error {
		t.Helper()
		obs, err := observe.Evaluate(observe.Block{
			Subjects: []observe.SubjectExpr{
				{ResourceType: `"github_pr"`, ResourceID: `"demo-org/demo-repo#6"`},
			},
			Facts: map[string]string{"probe": expr},
		}, map[string]any{"result": prItems()})
		require.NoError(t, err)
		require.Len(t, obs, 1)
		return factcontent.Record(ctx, m, scope, observedfact.KindName, observedfact.IDPrefix, obs[0])
	}

	const first = `result.items.map(i, {"id": i.id, "ok": i.ok})`
	require.NoError(t, record(t, first))

	// The same value re-derived from a later call is a repeat, not a conflict.
	require.NoError(t, record(t, first), "an identical re-observation must be absorbed")

	// A genuinely different list of maps must still collide loudly.
	err := record(t, `result.items.map(i, {"id": i.id, "ok": !i.ok})`)
	require.ErrorIs(t, err, memory.ErrAppendOnlyConflict)
}
