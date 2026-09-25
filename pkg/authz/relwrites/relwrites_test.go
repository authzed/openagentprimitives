package relwrites

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeWriter struct {
	got [][]ResolvedTuple
	err error
}

func (f *fakeWriter) WriteRelationships(_ context.Context, t []ResolvedTuple) error {
	f.got = append(f.got, t)
	return f.err
}

func TestRun_WritesEachBlockSeparately(t *testing.T) {
	blocks := []Block{
		{ForEach: "result.results", Tuple: Tuple{Resource: `"a:" + item.x`, Relation: `"r"`, Subject: `"b:" + item.y`}},
		{Tuple: Tuple{Resource: `"a:1"`, Relation: `"r"`, Subject: `"b:1"`}},
	}
	w := &fakeWriter{}
	Run(context.Background(), w, blocks, map[string]any{
		"result": map[string]any{
			"results": []any{map[string]any{"x": "9", "y": "10"}},
		},
	}, nil, func(string, ...any) {})
	require.Len(t, w.got, 2, "want 2 write batches")
	require.NotEmpty(t, w.got[0])
	assert.Equal(t, "a:9", w.got[0][0].Resource)
}

func TestRun_FailedWriteContinuesToNextBlock(t *testing.T) {
	blocks := []Block{
		{Tuple: Tuple{Resource: `"a:1"`, Relation: `"r"`, Subject: `"b:1"`}},
		{Tuple: Tuple{Resource: `"a:2"`, Relation: `"r"`, Subject: `"b:2"`}},
	}
	w := &fakeWriter{err: fmt.Errorf("spicedb down")}
	var logs []string
	Run(context.Background(), w, blocks, nil, nil, func(msg string, kv ...any) { logs = append(logs, msg) })
	assert.Len(t, w.got, 2, "want both blocks attempted")
	assert.Len(t, logs, 2, "want 2 error logs")
}

func TestEvaluate(t *testing.T) {
	cases := []struct {
		name    string
		block   Block
		args    map[string]any
		result  map[string]any
		want    []ResolvedTuple
		wantErr bool
	}{
		{
			name: "for_each expands per item with item.* + result.* refs",
			block: Block{
				When:    `has(result.results)`,
				ForEach: "result.results",
				Tuple: Tuple{
					Resource: `"crm_company:" + item.id`,
					Relation: `"hubspot_owner_id_ref"`,
					Subject:  `"hubspot_owner:" + item.properties.hubspot_owner_id`,
				},
			},
			args: map[string]any{},
			result: map[string]any{
				"results": []any{
					map[string]any{"id": "5083", "properties": map[string]any{"hubspot_owner_id": "9876"}},
					map[string]any{"id": "5084", "properties": map[string]any{"hubspot_owner_id": "9877"}},
				},
			},
			want: []ResolvedTuple{
				{Resource: "crm_company:5083", Relation: "hubspot_owner_id_ref", Subject: "hubspot_owner:9876"},
				{Resource: "crm_company:5084", Relation: "hubspot_owner_id_ref", Subject: "hubspot_owner:9877"},
			},
		},
		{
			name:  "when=false short-circuits, emits zero tuples",
			block: Block{When: "false", ForEach: "[1]", Tuple: Tuple{Resource: `"a:1"`, Relation: `"r"`, Subject: `"a:2"`}},
			want:  nil,
		},
		{
			name:  "no for_each: emit single tuple from args.*",
			block: Block{Tuple: Tuple{Resource: `"a:" + args.x`, Relation: `"r"`, Subject: `"b:" + args.y`}},
			args:  map[string]any{"x": "1", "y": "2"},
			want:  []ResolvedTuple{{Resource: "a:1", Relation: "r", Subject: "b:2"}},
		},
		{
			name:    "malformed tuple (empty resource) errors",
			block:   Block{Tuple: Tuple{Resource: `""`, Relation: `"r"`, Subject: `"b:1"`}},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Evaluate(tc.block, map[string]any{"args": tc.args, "result": tc.result})
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestEvaluate_SessionBinding(t *testing.T) {
	block := Block{
		Tuple: Tuple{
			Resource: `"cluster:" + args.argv[1]`,
			Relation: `"debug_target"`,
			Subject:  `"agentsession:" + session`,
		},
	}
	tuples, err := Evaluate(block, map[string]any{
		"args":    map[string]any{"argv": []any{"get-kubeconfig", "prod-example-1"}},
		"result":  map[string]any{"success": true},
		"session": "default/sre-session-1",
	})
	require.NoError(t, err)
	require.Len(t, tuples, 1)
	assert.Equal(t, ResolvedTuple{
		Resource: "cluster:prod-example-1",
		Relation: "debug_target",
		Subject:  "agentsession:default/sre-session-1",
	}, tuples[0])
}

// TestEvaluate_PropagatesExclusive proves block.Exclusive is stamped onto
// every emitted ResolvedTuple (single-tuple and forEach), and defaults false.
func TestEvaluate_PropagatesExclusive(t *testing.T) {
	t.Run("single tuple carries Exclusive=true", func(t *testing.T) {
		block := Block{
			Exclusive: true,
			Tuple: Tuple{
				Resource: `"cluster:" + args.argv[1]`,
				Relation: `"debug_target"`,
				Subject:  `"agentsession:" + session`,
			},
		}
		tuples, err := Evaluate(block, map[string]any{
			"args":    map[string]any{"argv": []any{"get-kubeconfig", "c1"}},
			"session": "ns/s",
		})
		require.NoError(t, err)
		require.Len(t, tuples, 1)
		assert.True(t, tuples[0].Exclusive, "Exclusive must propagate to the emitted tuple")
	})
	t.Run("forEach emits one Exclusive tuple per element", func(t *testing.T) {
		block := Block{
			Exclusive: true,
			ForEach:   "args.ids",
			Tuple: Tuple{
				Resource: `"cluster:" + item`,
				Relation: `"debug_target"`,
				Subject:  `"agentsession:" + session`,
			},
		}
		tuples, err := Evaluate(block, map[string]any{
			"args":    map[string]any{"ids": []any{"c1", "c2"}},
			"session": "ns/s",
		})
		require.NoError(t, err)
		require.Len(t, tuples, 2)
		for _, tu := range tuples {
			assert.True(t, tu.Exclusive, "every forEach tuple must carry Exclusive")
		}
	})
	t.Run("default is non-exclusive", func(t *testing.T) {
		block := Block{Tuple: Tuple{Resource: `"a:1"`, Relation: `"r"`, Subject: `"b:1"`}}
		tuples, err := Evaluate(block, map[string]any{})
		require.NoError(t, err)
		require.Len(t, tuples, 1)
		assert.False(t, tuples[0].Exclusive, "Exclusive defaults false")
	})
}

func TestEvaluate_MissingSessionBindingIsNilNotError(t *testing.T) {
	// Callers that don't supply session (e.g. a block that never
	// references it) must keep working: unreferenced vars cost nothing.
	block := Block{
		Tuple: Tuple{
			Resource: `"doc:" + args.id`,
			Relation: `"viewer"`,
			Subject:  `"user:" + result.user`,
		},
	}
	tuples, err := Evaluate(block, map[string]any{
		"args":   map[string]any{"id": "a"},
		"result": map[string]any{"user": "alice"},
	})
	require.NoError(t, err)
	require.Len(t, tuples, 1)
	assert.Equal(t, "doc:a", tuples[0].Resource)
}

// spicedb_user_id(email) must be available to relwrites expressions.
//
// This package forks its own CEL env (to add the session/call bindings the
// shared pkg/authz env doesn't declare) and, in forking, dropped the
// spicedb_user_id function library the shared env registers. Any MCPServer
// relationship-write expression using it therefore failed to COMPILE:
//
//	failed to register access-control relationships ...
//	CEL compile error: undeclared reference to 'spicedb_user_id'
//
// Observed in a live cluster: an agent could not fetch a company's contacts
// because the owner-access relationship could not be registered first, and the
// agent correctly refused to proceed without it. The function is the canonical
// way to turn an email into a SpiceDB user id, so a relationship subject that
// derives from an email address cannot be expressed without it.
func TestCELEnv_ProvidesSpiceDBUserIDFunction(t *testing.T) {
	cases := []struct {
		name string
		expr string
	}{
		{
			name: "subject derived from an item email",
			expr: `"user:" + spicedb_user_id(item.email)`,
		},
		{
			name: "nested property access, as a real MCPServer spec writes it",
			expr: `"user:" + spicedb_user_id(item.properties.hs_email)`,
		},
		{
			name: "composed with the session binding this package adds",
			expr: `"user:" + spicedb_user_id(session.email)`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := compileString(tc.expr)
			require.NoError(t, err,
				"relwrites CEL env must register spicedb_user_id, same as the shared pkg/authz env")
		})
	}
}

// The other bindings this package's env declares must keep working — the fix
// must add the function library without disturbing them.
func TestCELEnv_RetainsItsOwnBindings(t *testing.T) {
	for _, expr := range []string{
		`args.foo`, `result.bar`, `item.baz`, `session.qux`, `call.quux`,
	} {
		_, err := compileString(expr)
		assert.NoError(t, err, "binding must remain available: %s", expr)
	}
}

// ValidateBlock is the apply-time gate for MCPServer.writesRelationships.
// It MUST compile through this package's own celEnv, so a spec that validates
// is guaranteed to compile at execution time — the two must never drift.
//
// Before this existed, writesRelationships CEL was compiled for the FIRST time
// during a live tool call: an MCPServer carrying an uncompilable expression
// reported Valid=True, and the failure surfaced mid-turn as an unactionable
// error to the user. The sibling `labels` CEL surface was already validated at
// apply time; this closes the inconsistency.
func TestValidateBlock(t *testing.T) {
	good := Tuple{
		Resource: `"hubspot_company:" + item.id`,
		Relation: `"owner"`,
		Subject:  `"user:" + spicedb_user_id(item.properties.hs_email)`,
	}
	cases := []struct {
		name    string
		block   Block
		wantErr string
	}{
		{
			name:  "valid block using spicedb_user_id: accepted",
			block: Block{ForEach: `result.results`, Tuple: good},
		},
		{
			name:  "valid block with a when clause",
			block: Block{When: `result.total > 0`, ForEach: `result.results`, Tuple: good},
		},
		{
			name:    "undeclared function: rejected, naming the field",
			block:   Block{Tuple: Tuple{Resource: `"a:1"`, Relation: `"r"`, Subject: `"user:" + no_such_func("x")`}},
			wantErr: "tuple.subject",
		},
		{
			name:    "undeclared variable: rejected",
			block:   Block{Tuple: Tuple{Resource: `"a:" + nope.id`, Relation: `"r"`, Subject: `"b:1"`}},
			wantErr: "tuple.resource",
		},
		{
			name:    "malformed when clause: rejected",
			block:   Block{When: `result.total >`, Tuple: good},
			wantErr: "when",
		},
		{
			name:    "malformed forEach: rejected",
			block:   Block{ForEach: `result.(`, Tuple: good},
			wantErr: "forEach",
		},
		{
			name:    "non-bool when: rejected",
			block:   Block{When: `"a string"`, Tuple: good},
			wantErr: "when",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateBlock(tc.block)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr,
				"error must name the offending field so an author can fix it without source-diving")
		})
	}
}

// forEach must accept ANY CEL expression that evaluates to a list, not just a
// bare field reference.
//
// The implementation asserted v.Value().([]any). A plain field access
// (result.results) yields a Go []any and passed; a CEL list macro — filter,
// map — constructs a new list whose Value() is []ref.Val and failed with
// "forEach: expected list, got []ref.Val". So forEach silently supported only
// the simplest possible expression, and the documented way to skip records
// missing a property was unusable.
//
// Observed in a live cluster: filtering out companies with no owner (the fix
// for a different bug) made every search abort.
func TestEvaluate_ForEachAcceptsCELListMacros(t *testing.T) {
	vars := map[string]any{
		"result": map[string]any{
			"results": []any{
				map[string]any{"id": "1", "owner": "o1"},
				map[string]any{"id": "2"}, // no owner — the one a filter removes
				map[string]any{"id": "3", "owner": "o3"},
			},
		},
	}
	tuple := Tuple{
		Resource: `"crm_company:" + item.id`,
		Relation: `"owner"`,
		Subject:  `"hubspot_owner:" + item.owner`,
	}

	cases := []struct {
		name    string
		forEach string
		wantIDs []string
	}{
		{
			name:    "bare field reference (always worked)",
			forEach: `result.results.filter(i, has(i.owner))`,
			wantIDs: []string{"crm_company:1", "crm_company:3"},
		},
		{
			name:    "filter macro — the case that failed",
			forEach: `result.results.filter(i, has(i.owner))`,
			wantIDs: []string{"crm_company:1", "crm_company:3"},
		},
		{
			name:    "filter composed with a predicate on a value",
			forEach: `result.results.filter(i, has(i.owner) && i.owner != "o1")`,
			wantIDs: []string{"crm_company:3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Evaluate(Block{ForEach: tc.forEach, Tuple: tuple}, vars)
			require.NoError(t, err, "a list-valued CEL expression must be accepted")
			ids := make([]string, 0, len(got))
			for _, r := range got {
				ids = append(ids, r.Resource)
			}
			assert.Equal(t, tc.wantIDs, ids)
		})
	}
}

// A non-list forEach must still be a clear error rather than silently
// producing nothing.
func TestEvaluate_ForEachNonListStillErrors(t *testing.T) {
	_, err := Evaluate(Block{
		ForEach: `result.total`,
		Tuple:   Tuple{Resource: `"a:1"`, Relation: `"r"`, Subject: `"b:1"`},
	}, map[string]any{"result": map[string]any{"total": 5}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forEach", "the error must name the offending field")
}

// ResolveItems's nil-vs-non-nil result is load-bearing: both relwrites.Evaluate
// (below) and observe.Evaluate (pkg/authz/observe/evaluate.go, which shares
// this function per RULING R2) early-return on a literal nil to mean "the
// block does not fire, skip it entirely." That must never be confused with a
// ForEach that legitimately resolved to zero items, which still means "run
// the loop" — zero iterations of it, but not a skip. This pins the three
// shapes the doc comment on ResolveItems promises.
func TestResolveItems_NilVsEmptyContract(t *testing.T) {
	cases := []struct {
		name    string
		when    string
		forEach string
		vars    map[string]any
		want    []any // nil means "must be a literal nil slice"
	}{
		{
			name: "when=false: nil, regardless of what forEach would have matched",
			when: "false",
			// A non-empty forEach here proves the nil comes from the When
			// gate, not from forEach resolving to nothing.
			forEach: "[1]",
			want:    nil,
		},
		{
			name: "no forEach: exactly one item, nil-bound",
			want: []any{nil},
		},
		{
			name:    "forEach matches zero items: non-nil, empty",
			forEach: "result.items",
			vars:    map[string]any{"result": map[string]any{"items": []any{}}},
			want:    []any{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveItems(tc.when, tc.forEach, tc.vars)
			require.NoError(t, err)
			if tc.want == nil {
				assert.Nil(t, got, "when=false must return the literal nil sentinel, not merely an empty list")
				return
			}
			require.NotNil(t, got, "an item list must never be nil except via the when=false sentinel")
			assert.Equal(t, tc.want, got)
		})
	}
}

// EvalAnyWithItem is the one general-value path in this shared CEL surface —
// every other evaluator here asserts bool or string, where cel-go's Value() is
// already the plain Go value. This pins its contract at the package that owns
// it, so a future caller that is not pkg/authz/observe still gets JSON-shaped
// values; the end-to-end proof that the shape survives storage lives in
// pkg/authz/observe (roundtrip_test.go).
func TestEvalAnyWithItem_ReturnsJSONShapedValues(t *testing.T) {
	cases := []struct {
		name    string
		expr    string
		item    any
		want    any
		wantErr string
	}{
		{
			// A list MACRO's Value() is []ref.Val, and its elements are
			// map[ref.Val]ref.Val — neither is a Go value anything downstream
			// can encode. See celNative.
			name: "map macro over item: []any of map[string]any, all the way down",
			expr: `item.rows.map(r, {"id": r.id, "tags": [r.id]})`,
			item: map[string]any{"rows": []any{map[string]any{"id": "a"}}},
			want: []any{map[string]any{"id": "a", "tags": []any{"a"}}},
		},
		{
			// A plain field reference is already native. celconv.List's comment
			// explains why the two representations must both be covered.
			name: "plain field reference: same shape as the macro",
			expr: `item.rows`,
			item: map[string]any{"rows": []any{map[string]any{"id": "a"}}},
			want: []any{map[string]any{"id": "a"}},
		},
		{
			name: "map literal: map[string]any, nested list converted",
			expr: `{"outer": [{"inner": 1}]}`,
			want: map[string]any{"outer": []any{map[string]any{"inner": int64(1)}}},
		},
		{
			name:    "non-string map key: refused, naming the reason",
			expr:    `{1: "a"}`,
			wantErr: "JSON object keys are strings",
		},
		{name: "scalar bool is unchanged", expr: `false`, want: false},
		{name: "scalar int is unchanged", expr: `1 + 1`, want: int64(2)},
		{name: "scalar string is unchanged", expr: `"hi"`, want: "hi"},
		{name: "null becomes a Go nil, not a protobuf enum", expr: `null`, want: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prg, err := CompileAnyExpr(tc.expr)
			require.NoError(t, err)

			got, err := EvalAnyWithItem(prg, nil, tc.item)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
