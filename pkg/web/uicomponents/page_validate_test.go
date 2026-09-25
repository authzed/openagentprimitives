package uicomponents

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pageDecl builds a declaration whose view is the given JSON, normalized the
// way ParseDeclaration would leave it, so Validate sees what admission sees.
func pageDecl(t *testing.T, view string) Declaration {
	t.Helper()
	var n Node
	require.NoError(t, json.Unmarshal([]byte(view), &n))
	d, err := Normalize(Declaration{View: &n})
	require.NoError(t, err)
	return d
}

func TestValidate_PageRoot(t *testing.T) {
	cases := []struct {
		name    string
		view    string
		wantErr string // empty = valid
	}{
		{
			name: "oap:page at the root with layout rail: valid",
			view: `{"component":"oap:page","props":{"layout":"rail"},"children":[{"component":"ap:heading","props":{"text":"t"}}]}`,
		},
		{
			name: "oap:page at the root with no layout: valid (column)",
			view: `{"component":"oap:page","children":[{"component":"ap:heading","props":{"text":"t"}}]}`,
		},
		{
			name:    "oap:page nested under a stack: refused, root only",
			view:    `{"component":"ap:stack","children":[{"component":"oap:page","props":{"layout":"rail"}}]}`,
			wantErr: "oap:page may only be the root",
		},
		{
			name:    "two oap:page nodes: refused",
			view:    `{"component":"oap:page","children":[{"component":"oap:page"}]}`,
			wantErr: "oap:page may only be the root",
		},
		{
			name:    "unknown layout: refused by the value check",
			view:    `{"component":"oap:page","props":{"layout":"grid"}}`,
			wantErr: `oap:page: layout "grid"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(pageDecl(t, tc.view), DefaultOptions())
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestValidate_PageIsStructural(t *testing.T) {
	// Never published to the agent, never legal in a fill: the same two
	// properties oap:generative has, inherited from Component.Structural.
	raw, err := VocabularySchema()
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"oap:page"`, "a structural type is not in the agent's vocabulary")

	base := pageDecl(t, `{"component":"ap:stack","children":[{"component":"oap:generative","props":{"name":"r","allowedComponents":["*"]}}]}`)
	fill := Node{Component: PageType}
	v := ResolveView(base, []Fragment{{Hook: "r", Node: &fill}}, DefaultOptions())
	require.Len(t, v.Rejected, 1)
	assert.Contains(t, v.Rejected[0].Reason, "oap:page")
}

func TestCheckSteps_Summary(t *testing.T) {
	long := strings.Repeat("x", MaxStepSummaryLen+1)
	cases := []struct {
		name    string
		steps   []Step
		wantErr string
	}{
		{name: "summary within the limit: valid", steps: []Step{{ID: "a", Label: "A", Summary: "tell me what to build"}}},
		{name: "summary over the limit: refused by rune count", steps: []Step{{ID: "a", Label: "A", Summary: long}}, wantErr: "summary is 61 characters"},
		{name: "summary with a newline: refused, one line only", steps: []Step{{ID: "a", Label: "A", Summary: "two\nlines"}}, wantErr: "one line"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkSteps(&StepsProps{Steps: tc.steps})
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
