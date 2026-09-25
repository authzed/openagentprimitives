package agentclass

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestValidateRoster(t *testing.T) {
	cases := []struct {
		name     string
		root     string
		rosters  map[string][]string
		maxDepth int
		wantErr  string
	}{
		{
			name:     "no roster: valid",
			root:     "demo-solo",
			rosters:  map[string][]string{"demo-solo": nil},
			maxDepth: 3,
		},
		{
			name:     "flat roster: valid",
			root:     "demo-lead",
			rosters:  map[string][]string{"demo-lead": {"demo-coder"}, "demo-coder": nil},
			maxDepth: 3,
		},
		{
			name:     "direct cycle",
			root:     "demo-lead",
			rosters:  map[string][]string{"demo-lead": {"demo-coder"}, "demo-coder": {"demo-lead"}},
			maxDepth: 3,
			wantErr:  "delegation cycle",
		},
		{
			name:     "self reference is a cycle",
			root:     "demo-lead",
			rosters:  map[string][]string{"demo-lead": {"demo-lead"}},
			maxDepth: 3,
			wantErr:  "delegation cycle",
		},
		{
			name:     "depth exceeded",
			root:     "a",
			rosters:  map[string][]string{"a": {"b"}, "b": {"c"}, "c": {"d"}, "d": nil},
			maxDepth: 2,
			wantErr:  "exceeds the maximum delegation depth",
		},
		{
			name:     "diamond is not a cycle",
			root:     "a",
			rosters:  map[string][]string{"a": {"b", "c"}, "b": {"d"}, "c": {"d"}, "d": nil},
			maxDepth: 3,
		},
		{
			name:     "unknown member names the missing class",
			root:     "demo-lead",
			rosters:  map[string][]string{"demo-lead": {"demo-absent"}},
			maxDepth: 3,
			wantErr:  "demo-absent",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRoster(tc.root, tc.rosters, tc.maxDepth)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestValidateSubagentModes(t *testing.T) {
	class := func(roster []string, modes map[string][]string) *spiceboxv1alpha1.AgentClass {
		return &spiceboxv1alpha1.AgentClass{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "demo-lead"},
			Spec:       spiceboxv1alpha1.AgentClassSpec{Subagents: roster, SubagentModes: modes},
		}
	}
	cases := []struct {
		name     string
		ac       *spiceboxv1alpha1.AgentClass
		wantMsgs []string // empty means the declaration must be accepted
	}{
		{
			// The Track 1a shape. Every manifest written before spec.subagentModes
			// existed looks exactly like this, and it must keep validating.
			name: "old-shape []string roster, no modes map: accepted",
			ac:   class([]string{"demo-coder", "demo-sre"}, nil),
		},
		{
			name: "no roster and no modes map: accepted",
			ac:   class(nil, nil),
		},
		{
			name: "every key on the roster, every value a mode: accepted",
			ac: class([]string{"demo-coder", "demo-sre"}, map[string][]string{
				"demo-coder": {spiceboxv1alpha1.SubagentModeChat},
				"demo-sre":   {spiceboxv1alpha1.SubagentModeTask, spiceboxv1alpha1.SubagentModeSingleTurn},
			}),
		},
		{
			name: "an empty mode list widens nothing and is accepted",
			ac:   class([]string{"demo-coder"}, map[string][]string{"demo-coder": {}}),
		},
		{
			name:     "key absent from spec.subagents: refused naming the dead entry",
			ac:       class([]string{"demo-coder"}, map[string][]string{"demo-sre": {spiceboxv1alpha1.SubagentModeChat}}),
			wantMsgs: []string{"demo-sre", "not in spec.subagents"},
		},
		{
			// The shape the unconditional call site exists for: gating this
			// check on a non-empty roster would let exactly this one through.
			name:     "modes declared with no roster at all: refused",
			ac:       class(nil, map[string][]string{"demo-coder": {spiceboxv1alpha1.SubagentModeChat}}),
			wantMsgs: []string{"demo-coder", "not in spec.subagents"},
		},
		{
			name:     "misspelled mode: refused naming the value and the valid set",
			ac:       class([]string{"demo-coder"}, map[string][]string{"demo-coder": {"chatt"}}),
			wantMsgs: []string{"chatt", "not a delegation mode", "single_turn"},
		},
		{
			name:     "empty-string mode: refused rather than read as the default",
			ac:       class([]string{"demo-coder"}, map[string][]string{"demo-coder": {""}}),
			wantMsgs: []string{"not a delegation mode"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, msg := validateSubagentModes(tc.ac)
			if len(tc.wantMsgs) == 0 {
				assert.Empty(t, reason, "a sound declaration must not mark the class invalid")
				assert.Empty(t, msg)
				return
			}
			require.Equal(t, spiceboxv1alpha1.ReasonRosterInvalid, reason,
				"a roster-shaped misconfiguration reuses the roster reason rather than minting a new one")
			for _, want := range tc.wantMsgs {
				assert.Contains(t, msg, want)
			}
		})
	}
}

// TestValidateSubagentModes_MessageIsStableAcrossRuns pins the sort in
// validateSubagentModes. Go randomizes map iteration, so an unsorted walk over
// a class with several bad entries would report a different one on each
// reconcile and rewrite the class's status condition every pass.
func TestValidateSubagentModes_MessageIsStableAcrossRuns(t *testing.T) {
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "demo-lead"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			SubagentModes: map[string][]string{
				"demo-alpha": {spiceboxv1alpha1.SubagentModeChat},
				"demo-bravo": {spiceboxv1alpha1.SubagentModeChat},
				"demo-delta": {spiceboxv1alpha1.SubagentModeChat},
			},
		},
	}
	_, first := validateSubagentModes(ac)
	require.NotEmpty(t, first, "every key here is off-roster, so this must be refused")
	for range 20 {
		_, again := validateSubagentModes(ac)
		require.Equal(t, first, again, "the reported offender must not depend on map iteration order")
	}
}

// TestSubagentsCapabilityProblem is MAJOR-2's durable half: the documented
// build flow (builder-agent skill) adds a roster entry without necessarily
// granting the subagents capability in the same step, which leaves a class
// that reconciles Valid=True but can never actually delegate — nothing
// warns the next author. This pins the pure check that feeds
// AgentClassConditionSubagentsCapabilityGranted.
func TestSubagentsCapabilityProblem(t *testing.T) {
	cases := []struct {
		name       string
		subagents  []string
		caps       map[string]apiextensionsv1.JSON
		wantReason string
	}{
		{
			name: "no roster: nothing to warn about",
		},
		{
			name:      "roster present, capability granted: no warning",
			subagents: []string{"demo-coder"},
			caps:      map[string]apiextensionsv1.JSON{"subagents": {Raw: []byte(`{}`)}},
		},
		{
			name:      "roster present, capability explicitly disabled: still no warning — an author's deliberate choice, not an omission",
			subagents: []string{"demo-coder"},
			caps:      map[string]apiextensionsv1.JSON{"subagents": {Raw: []byte(`{"enabled":false}`)}},
		},
		{
			name:       "roster present, capability entry entirely absent: warns",
			subagents:  []string{"demo-coder"},
			wantReason: spiceboxv1alpha1.ReasonSubagentsCapabilityMissing,
		},
		{
			name:       "roster present, ONLY an unrelated capability granted: still warns",
			subagents:  []string{"demo-coder", "demo-writer"},
			caps:       map[string]apiextensionsv1.JSON{"knowledge": {Raw: []byte(`{}`)}},
			wantReason: spiceboxv1alpha1.ReasonSubagentsCapabilityMissing,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ac := &spiceboxv1alpha1.AgentClass{
				Spec: spiceboxv1alpha1.AgentClassSpec{Subagents: tc.subagents, Capabilities: tc.caps},
			}
			reason, msg := subagentsCapabilityProblem(ac)
			if tc.wantReason == "" {
				assert.Empty(t, reason, "must not warn: msg=%q", msg)
				assert.Empty(t, msg)
				return
			}
			assert.Equal(t, tc.wantReason, reason)
			assert.Contains(t, msg, "subagents", "the message must name the missing capability")
			assert.Contains(t, msg, "delegate", "the message must say what the omission actually costs")
		})
	}
}
