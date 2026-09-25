package authz

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompileBool_EvaluatesPredicate(t *testing.T) {
	prg, err := CompileBool(`args.objectType == "contacts"`)
	require.NoError(t, err, "compile")

	cases := []struct {
		name string
		args map[string]any
		want bool
	}{
		{name: "matching value returns true", args: map[string]any{"objectType": "contacts"}, want: true},
		{name: "non-matching value returns false", args: map[string]any{"objectType": "companies"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EvalBool(prg, tc.args)
			require.NoError(t, err, "eval")
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCompileBool_SyntaxError(t *testing.T) {
	_, err := CompileBool("this is not valid CEL %%%")
	require.Error(t, err, "want CEL compile error")
	assert.Contains(t, err.Error(), "CEL")
}

func TestCompileString_NestedExtraction(t *testing.T) {
	expr := `args.filterGroups[0].filters.filter(f, f.propertyName == "associations.company")[0].value`
	prg, err := CompileString(expr)
	require.NoError(t, err, "compile")
	got, err := EvalString(prg, map[string]any{
		"filterGroups": []any{map[string]any{
			"filters": []any{
				map[string]any{"propertyName": "createdate", "value": "2026-01-01"},
				map[string]any{"propertyName": "associations.company", "value": "5083920582"},
			},
		}},
	})
	require.NoError(t, err, "eval")
	assert.Equal(t, "5083920582", got)
}

func TestEvalString_EmptyResultIsError(t *testing.T) {
	prg, err := CompileString(`args.maybeMissing`)
	require.NoError(t, err, "compile")
	_, err = EvalString(prg, map[string]any{})
	require.Error(t, err, "missing args.maybeMissing should error")
}
