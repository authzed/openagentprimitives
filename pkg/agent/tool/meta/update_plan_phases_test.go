package meta_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state/plans"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
)

// phasesSession reuses the existing update_plan fixture rather than building a
// parallel one — same registry, same operations, same note capture.
func phasesSession(t *testing.T) *tool.SessionContext {
	t.Helper()
	return newSessForUpdatePlanTest(t)
}

func runUpdatePlan(t *testing.T, sess *tool.SessionContext, args string) tool.Result {
	t.Helper()
	res, err := meta.NewUpdatePlan(meta.UpdatePlanConfig{}).
		Execute(context.Background(), json.RawMessage(args), sess)
	require.NoError(t, err, "Execute must not return a Go error for agent-input problems")
	return res
}

const wellFormedPhasePlan = `{
  "name":"main",
  "items":[{"id":"s1","label":"read it","status":"pending","phase":"recon"}],
  "phases":[
    {"id":"recon","label":"Read the issue","why":"I need the body before deciding",
     "permissions":[{"handle":"perm:read:tracker_issue","why":"to read the issue"}]},
    {"id":"write","label":"Apply the fix","why":"make the change the issue asks for",
     "requires":[{"phase":"recon","why":"must read before editing"}],
     "max":{"count":2,"why":"one edit plus one retry"},
     "permissions":[{"handle":"perm:write:tracker_issue","why":"to post the fix"}]}
  ]
}`

func TestUpdatePlan_acceptsAWellFormedMultiPhasePlan(t *testing.T) {
	sess := phasesSession(t)

	res := runUpdatePlan(t, sess, wellFormedPhasePlan)
	require.False(t, res.IsError, "well-formed plan rejected: %s", res.Content)

	got, ok := plans.From(sess).Get("main")
	require.True(t, ok)
	require.Len(t, got.Phases, 2)

	assert.Equal(t, "recon", got.Phases[0].ID)
	assert.Equal(t, "write", got.Phases[1].ID)
	require.Len(t, got.Phases[1].Requires, 1)
	assert.Equal(t, "recon", got.Phases[1].Requires[0].Phase)
	require.NotNil(t, got.Phases[1].Max)
	assert.Equal(t, 2, got.Phases[1].Max.Count)
	assert.Equal(t, "recon", got.Items[0].Phase, "the item's display grouping must persist")
}

// Order is authority once frozen — requires-edges resolve to indices — so the
// declared order must survive the tool boundary exactly.
func TestUpdatePlan_preservesPhaseDeclarationOrder(t *testing.T) {
	sess := phasesSession(t)
	require.False(t, runUpdatePlan(t, sess, wellFormedPhasePlan).IsError)

	got, _ := plans.From(sess).Get("main")
	require.Len(t, got.Phases, 2)
	assert.Equal(t, []string{"recon", "write"}, []string{got.Phases[0].ID, got.Phases[1].ID})
}

// A permission with no justification is refused. The whole point of the field
// is that a wide ceiling has to be argued for, in the agent's own words, on the
// card an approver reads.
func TestUpdatePlan_rejectsAPermissionWithNoWhy(t *testing.T) {
	sess := phasesSession(t)

	res := runUpdatePlan(t, sess, `{
      "name":"main","items":[],
      "phases":[{"id":"p","label":"L","why":"w",
        "permissions":[{"handle":"perm:read:tracker_issue"}]}]
    }`)

	assert.True(t, res.IsError, "a permission without why must be rejected")
	assert.Contains(t, res.Content, "why")
}

func TestUpdatePlan_rejectsMalformedPhases(t *testing.T) {
	cases := []struct {
		name, args, wantIn string
	}{
		{
			name:   "phase with no id",
			args:   `{"name":"main","items":[],"phases":[{"label":"L","why":"w"}]}`,
			wantIn: "id",
		},
		{
			name:   "phase with no why",
			args:   `{"name":"main","items":[],"phases":[{"id":"p","label":"L"}]}`,
			wantIn: "why",
		},
		{
			name:   "requires edge with no why",
			args:   `{"name":"main","items":[],"phases":[{"id":"p","label":"L","why":"w","requires":[{"phase":"q"}]}]}`,
			wantIn: "why",
		},
		{
			name:   "max with no why",
			args:   `{"name":"main","items":[],"phases":[{"id":"p","label":"L","why":"w","max":{"count":2}}]}`,
			wantIn: "why",
		},
		{
			name:   "max count below 1 — a phase that could never be entered",
			args:   `{"name":"main","items":[],"phases":[{"id":"p","label":"L","why":"w","max":{"count":0,"why":"x"}}]}`,
			wantIn: "count",
		},
		{
			name:   "duplicate phase id — requires edges would be ambiguous",
			args:   `{"name":"main","items":[],"phases":[{"id":"p","label":"L","why":"w"},{"id":"p","label":"M","why":"w"}]}`,
			wantIn: "duplicate",
		},
		{
			name:   "requires naming a phase that does not exist",
			args:   `{"name":"main","items":[],"phases":[{"id":"p","label":"L","why":"w","requires":[{"phase":"nope","why":"x"}]}]}`,
			wantIn: "nope",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name+": rejected", func(t *testing.T) {
			res := runUpdatePlan(t, phasesSession(t), tc.args)
			require.True(t, res.IsError, "expected rejection, got: %s", res.Content)
			assert.Contains(t, res.Content, tc.wantIn,
				"the error must name what is wrong so the agent can fix it")
		})
	}
}

// A plan with no phases is the slice-1 shape and must keep working untouched.
func TestUpdatePlan_phaselessPlanStillWorks(t *testing.T) {
	sess := phasesSession(t)

	res := runUpdatePlan(t, sess, `{"name":"main","items":[{"id":"s1","label":"x","status":"pending"}]}`)
	require.False(t, res.IsError, "%s", res.Content)

	got, ok := plans.From(sess).Get("main")
	require.True(t, ok)
	assert.Empty(t, got.Phases)
}

// Exceeding a limit is a tool error NAMING the limit, so the agent re-plans
// smaller. Never a silent truncation, which would drop declared reach while
// leaving the plan looking complete.
func TestUpdatePlan_exceedingMaxPhasesErrorsNamingTheLimit(t *testing.T) {
	var phases []map[string]any
	for i := 0; i < meta.MaxPhasesForTest+1; i++ {
		phases = append(phases, map[string]any{
			"id": "p" + string(rune('a'+i%26)) + string(rune('a'+i/26)), "label": "L", "why": "w",
		})
	}
	body, err := json.Marshal(map[string]any{"name": "main", "items": []any{}, "phases": phases})
	require.NoError(t, err)

	res := runUpdatePlan(t, phasesSession(t), string(body))

	require.True(t, res.IsError)
	assert.Contains(t, res.Content, "maxPhases")
	assert.Contains(t, res.Content, "re-plan", "the error must tell the agent what to do next")
}

// An unknown phase field is IGNORED, not rejected — tool.ParseArgs uses plain
// json.Unmarshal for every tool in the repo, so `additionalProperties:false`
// in the schema is guidance to the model rather than a server-side check.
//
// That is safe, and this test pins the reason: the field is dropped at the
// decode boundary and never reaches plans.Phase, so it cannot smuggle
// authority. An agent writing `"approved": true` gets a plan with no such
// concept, not a plan that believes itself approved.
//
// The stronger behavior (DisallowUnknownFields) is deliberately NOT adopted
// here: it would change decoding for every tool at once and start failing
// agents mid-session over harmless extra keys, which is a bigger blast radius
// than the problem justifies.
func TestUpdatePlan_unknownPhaseFieldIsIgnoredAndCarriesNoAuthority(t *testing.T) {
	sess := phasesSession(t)

	// Note the item: `items: []` DELETES a plan (the long-standing contract),
	// so phases only persist on a plan that has at least one item.
	res := runUpdatePlan(t, sess, `{
      "name":"main","items":[{"id":"s1","label":"x","status":"pending"}],
      "phases":[{"id":"p","label":"L","why":"w","approved":true,"ceiling":["perm:write:anything"]}]
    }`)
	require.False(t, res.IsError, "an unknown field is ignored, not an error: %s", res.Content)

	got, ok := plans.From(sess).Get("main")
	require.True(t, ok)
	require.Len(t, got.Phases, 1)

	// The smuggled keys landed nowhere: the phase has exactly what the schema
	// defines, and no permissions it did not legitimately declare.
	assert.Equal(t, "p", got.Phases[0].ID)
	assert.Empty(t, got.Phases[0].Permissions,
		"a field the schema does not define must not become authority")

	b, err := json.Marshal(got.Phases[0])
	require.NoError(t, err)
	assert.NotContains(t, string(b), "approved")
	assert.NotContains(t, string(b), "ceiling")
}
