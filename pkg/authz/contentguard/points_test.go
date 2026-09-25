// The closed inspection-point set and its single Point→content mapping are what
// keep a consumer from covering one point and silently skipping another. These
// tests fail when the two drift.
package contentguard

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// TestSubjectForCoversEveryInspectionPoint: every declared point yields a
// Subject carrying real content. Adding a point to InspectionPoints without
// teaching SubjectFor fails here rather than handing an inspector an empty
// subject at runtime — which it would Pass (fail-open).
func TestSubjectForCoversEveryInspectionPoint(t *testing.T) {
	require.NotEmpty(t, InspectionPoints(), "the closed set must not be empty")
	for _, p := range InspectionPoints() {
		t.Run(string(p), func(t *testing.T) {
			s, err := SubjectFor(p, "some_tool", json.RawMessage(`{"a":1}`), "result text", false)
			require.NoError(t, err)
			assert.Equal(t, p, s.Point)
			assert.Equal(t, "some_tool", s.ToolName)
			assert.True(t, len(s.Args) > 0 || s.Result != "",
				"point %q must map onto some content", p)
		})
	}
}

// TestSubjectForRejectsAPointOutsideTheSet: an unknown point is an error, never
// an empty Subject, so a caller cannot inspect nothing and read it as a Pass.
func TestSubjectForRejectsAPointOutsideTheSet(t *testing.T) {
	s, err := SubjectFor(pipeline.SessionStart, "some_tool", json.RawMessage(`{}`), "text", false)
	require.Error(t, err)
	assert.Equal(t, Subject{}, s, "a refused point must yield no subject at all")
	assert.Contains(t, err.Error(), string(pipeline.SessionStart))
}

// TestInspectionPointsIsNotAliased: the contract cannot be mutated by a caller
// ranging over it.
func TestInspectionPointsIsNotAliased(t *testing.T) {
	first := InspectionPoints()
	require.NotEmpty(t, first)
	first[0] = pipeline.SessionEnd
	assert.NotEqual(t, pipeline.SessionEnd, InspectionPoints()[0], "InspectionPoints must return a fresh slice")
}
