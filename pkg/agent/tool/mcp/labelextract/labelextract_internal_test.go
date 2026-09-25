package labelextract

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEvalStringSoft_MissingKeySoftMiss is the canary for the
// substring-match "no such key" handling in evalStringSoft. cel-go
// formats missing-map-key errors as "no such key: <key>" inline, with
// no exported predicate to detect them by structure. If the cel-go
// wording ever changes (a version upgrade, a refactor in their
// evaluator), this test fails — that's the signal to update the
// substring match in evalStringSoft. Without this test, the change
// would be silent: TestEvaluate_MissingFieldsSkipsTuple would start
// surfacing errors, but the failure would point at the row-skip
// behavior rather than the underlying error-text detection.
func TestEvalStringSoft_MissingKeySoftMiss(t *testing.T) {
	prg, err := compileString(`item.missing_field`)
	require.NoError(t, err)

	got, err := evalStringSoft(prg, nil, nil, map[string]any{"present": "x"})
	assert.NoError(t, err, "missing key should be a soft miss, not an error")
	assert.Equal(t, "", got, "soft miss returns empty string")
}

// TestEvalStringSoft_NonStringResultErrors confirms that a CEL
// expression evaluating to a non-string non-nil value (e.g. a number)
// surfaces as an error rather than a silent skip — only the two
// canonical "missing" cases (missing-key, nil value) are soft.
func TestEvalStringSoft_NonStringResultErrors(t *testing.T) {
	// compileString accepts dyn outputs at compile time; the type
	// mismatch is caught at eval inside evalStringSoft.
	prg, err := compileString(`item.x`)
	require.NoError(t, err)

	_, err = evalStringSoft(prg, nil, nil, map[string]any{"x": 42})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected string")
}
