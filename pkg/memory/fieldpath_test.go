package memory_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

type schemaContent struct {
	TurnIndex int       `json:"turnIndex"`
	Status    string    `json:"status"`
	Secret    string    `json:"-"`
	Untagged  bool      ``
	At        time.Time `json:"at,omitempty"`
	unexposed string    //nolint:unused // present to prove unexported fields are not keys
}

type embedded struct {
	Inner string `json:"inner"`
}

type embeddingContent struct {
	embedded
	Named embedded `json:"named"`
	Own   string   `json:"own"`
}

// schemaKind is a test-local Kind with a real ContentSchema, so the facade has
// a content-key vocabulary to validate FieldEquals paths against.
type schemaKind struct {
	name, prefix string
	schema       reflect.Type
}

func (k schemaKind) Name() string                { return k.name }
func (k schemaKind) IDPrefix() string            { return k.prefix }
func (schemaKind) Retention() memory.Retention   { return memory.Retention{} }
func (k schemaKind) ContentSchema() reflect.Type { return k.schema }
func (schemaKind) IndexedFields() []string       { return nil }

// WriteAuthority: a stand-in for an ordinary agent-authored kind; the
// per-kind write door is exercised against the real registry in
// pkg/memory/httpsrv and pkg/memory/kinds/all.
func (schemaKind) WriteAuthority() memory.WriteAuthority        { return memory.SessionWritten }
func (schemaKind) NewScopeHooks(memory.Scope) memory.ScopeHooks { return noopHooks{} }

func TestContentKeys(t *testing.T) {
	cases := []struct {
		name string
		typ  reflect.Type
		want []string
	}{
		{
			name: "json tags win, `-` is omitted, untagged field keeps its Go name",
			typ:  reflect.TypeOf(schemaContent{}),
			want: []string{"turnIndex", "status", "Untagged", "at"},
		},
		{
			name: "unnamed embedded struct is flattened, named one contributes its name",
			typ:  reflect.TypeOf(embeddingContent{}),
			want: []string{"inner", "named", "own"},
		},
		{
			name: "pointer-to-struct resolves to the struct",
			typ:  reflect.TypeOf(&embedded{}),
			want: []string{"inner"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := memory.ContentKeys(tc.typ)
			require.NotNil(t, got)
			assert.Len(t, got, len(tc.want))
			for _, k := range tc.want {
				assert.Contains(t, got, k)
			}
		})
	}
}

// TestContentKeys_NilForUnknowableSchema pins the distinction callers depend
// on: nil means "no vocabulary is knowable", which must not be read as "this
// Kind has no keys" — doing so would turn every predicate into an error.
func TestContentKeys_NilForUnknowableSchema(t *testing.T) {
	assert.Nil(t, memory.ContentKeys(nil), "a Kind with opaque content")
	assert.Nil(t, memory.ContentKeys(reflect.TypeOf("")), "a non-struct schema")
}

func TestQuery_RejectsFieldPathThatNamesNoContentKey(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(schemaKind{name: "sk", prefix: "sk-", schema: reflect.TypeOf(schemaContent{})})
	mem := memory.NewLocal(inmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")
	scope := memory.Scope{Kind: "session", ID: "ns/a"}

	_, err := mem.Query(ctx, memory.Query{
		Scope: scope, Kinds: []string{"sk"},
		FieldEquals: []memory.FieldFilter{{Path: "TurnIndex", Value: 1}},
	})
	require.Error(t, err, "a Go field name names no stored key and can never match")
	assert.Contains(t, err.Error(), `"TurnIndex"`)
	assert.Contains(t, err.Error(), `did you mean "turnIndex"?`,
		"the case-only slip is the one this guard exists to catch")
}

func TestQuery_AcceptsFieldPathThatNamesAContentKey(t *testing.T) {
	t.Cleanup(memory.ResetRegistryForTest)
	memory.RegisterKind(schemaKind{name: "sk", prefix: "sk-", schema: reflect.TypeOf(schemaContent{})})
	mem := memory.NewLocal(inmem.NewBackend())
	ctx := memory.WithSystemApproval(context.Background(), "test")

	_, err := mem.Query(ctx, memory.Query{
		Scope: memory.Scope{Kind: "session", ID: "ns/a"}, Kinds: []string{"sk"},
		FieldEquals: []memory.FieldFilter{{Path: "turnIndex", Value: 1}},
	})
	assert.NoError(t, err)
}

// TestQuery_FieldPathValidationFailsOpenWhenUnknowable pins the deliberate
// limits of the guard: with nothing to resolve the path against it must let
// the query through rather than invent a rejection.
func TestQuery_FieldPathValidationFailsOpenWhenUnknowable(t *testing.T) {
	cases := []struct {
		name  string
		kinds []string
	}{
		{name: "no Kinds named: entries of any Kind could carry the key", kinds: nil},
		{name: "an unregistered Kind: its schema is unknown here", kinds: []string{"sk", "ghost"}},
		{name: "a Kind with opaque content: it may carry any key", kinds: []string{"sk", "opaque"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(memory.ResetRegistryForTest)
			memory.RegisterKind(schemaKind{name: "sk", prefix: "sk-", schema: reflect.TypeOf(schemaContent{})})
			memory.RegisterKind(schemaKind{name: "opaque", prefix: "op-", schema: nil})
			mem := memory.NewLocal(inmem.NewBackend())
			ctx := memory.WithSystemApproval(context.Background(), "test")

			_, err := mem.Query(ctx, memory.Query{
				Scope: memory.Scope{Kind: "session", ID: "ns/a"}, Kinds: tc.kinds,
				FieldEquals: []memory.FieldFilter{{Path: "TurnIndex", Value: 1}},
			})
			assert.NoError(t, err)
		})
	}
}
