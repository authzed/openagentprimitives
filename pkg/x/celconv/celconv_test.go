package celconv_test

import (
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types/ref"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/x/celconv"
)

// eval compiles and evaluates expr against a single `v` binding and returns the
// raw ref.Val, so each case hands List cel-go's own representation rather than
// a value the test constructed by hand.
func eval(t *testing.T, expr string, v any) ref.Val {
	t.Helper()
	env, err := cel.NewEnv(cel.Variable("v", cel.DynType))
	require.NoError(t, err, "cel env")
	ast, iss := env.Compile(expr)
	require.NoError(t, iss.Err(), "compile %q", expr)
	prg, err := env.Program(ast)
	require.NoError(t, err, "program %q", expr)
	out, _, err := prg.Eval(map[string]any{"v": v})
	require.NoError(t, err, "eval %q", expr)
	return out
}

func TestList(t *testing.T) {
	list := []any{"a", "b", "c"}

	cases := []struct {
		name    string
		expr    string
		want    []any
		wantErr string
	}{
		{
			// The one representation a raw .([]any) assertion also handled.
			name: "plain field reference: already a native []any",
			expr: `v`,
			want: []any{"a", "b", "c"},
		},
		{
			// The regression: a macro's list has Value() == []ref.Val, so the
			// native assertion rejected it. This is why List exists.
			name: "filter macro: []ref.Val is converted, not rejected",
			expr: `v.filter(x, x != "b")`,
			want: []any{"a", "c"},
		},
		{
			name: "map macro: []ref.Val is converted, not rejected",
			expr: `v.map(x, x + "!")`,
			want: []any{"a!", "b!", "c!"},
		},
		{
			name: "empty macro result: an empty slice, never a nil slice",
			expr: `v.filter(x, false)`,
			want: []any{},
		},
		{
			name:    "not a list at all: reports the Go type it actually got",
			expr:    `"scalar"`,
			wantErr: "expected list, got string",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := celconv.List(eval(t, tc.expr, list))
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.NotNil(t, got, "a valid list must never convert to a nil slice")
		})
	}
}
