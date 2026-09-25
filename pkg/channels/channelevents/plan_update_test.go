package channelevents_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

func TestKindPlanUpdate_ValidAndImplemented(t *testing.T) {
	require.True(t, channelevents.KindPlanUpdate.Valid())
	require.True(t, channelevents.KindPlanUpdate.Implemented())
}

func TestPlanUpdatePayload_Validate_AllowsEmptyItems(t *testing.T) {
	// Empty items signals deletion — it's valid on the wire.
	p := channelevents.PlanUpdatePayload{
		PlanName:  "main",
		Items:     nil,
		UpdatedAt: time.Now().UTC(),
	}
	require.NoError(t, p.Validate())
}

func TestPlanUpdatePayload_Validate_RejectsEmptyName(t *testing.T) {
	p := channelevents.PlanUpdatePayload{}
	require.Error(t, p.Validate())
}

func TestPlanUpdatePayload_Validate_RejectsParentItemWithoutPlan(t *testing.T) {
	p := channelevents.PlanUpdatePayload{
		PlanName:   "main",
		ParentItem: "x",
	}
	require.Error(t, p.Validate())
}

func TestPlanUpdatePayload_PublishOut_RoundTrip(t *testing.T) {
	var sent struct {
		subject string
		data    []byte
	}
	publish := func(subject string, data []byte) error {
		sent.subject = subject
		sent.data = append([]byte(nil), data...)
		return nil
	}
	pl := channelevents.PlanUpdatePayload{
		PlanName: "main",
		Items: []channelevents.PlanItemRef{
			{ID: "a", Label: "A", Status: "in_progress", OperationID: "op-1"},
		},
		UpdatedAt: time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC),
	}
	require.NoError(t, channelevents.PublishOut(publish, "default", "sess1",
		channelevents.KindPlanUpdate, pl))

	require.Equal(t, "ap.session.default.sess1.out.plan_update", sent.subject)
	require.Contains(t, string(sent.data), `"kind":"plan_update"`)
}

func TestPlanItemRef_JSONRoundtripWithDetailsOutputError(t *testing.T) {
	in := channelevents.PlanUpdatePayload{
		PlanName: "main",
		Items: []channelevents.PlanItemRef{
			{ID: "a", Label: "A", Status: "error", Details: "why it failed", Output: "stderr blob"},
		},
	}
	body, err := json.Marshal(in)
	require.NoError(t, err)

	var out channelevents.PlanUpdatePayload
	require.NoError(t, json.Unmarshal(body, &out))
	require.Equal(t, in.Items[0].Details, out.Items[0].Details)
	require.Equal(t, in.Items[0].Output, out.Items[0].Output)
	require.Equal(t, "error", out.Items[0].Status)
}

func TestPlanItemRef_OmitsEmptyDetailsOutput(t *testing.T) {
	in := channelevents.PlanUpdatePayload{
		PlanName: "main",
		Items:    []channelevents.PlanItemRef{{ID: "a", Label: "A", Status: "pending"}},
	}
	body, err := json.Marshal(in)
	require.NoError(t, err)
	require.NotContains(t, string(body), `"details"`)
	require.NotContains(t, string(body), `"output"`)
}

func TestPlanUpdatePayload_PausedDefaultsActiveAndOmitted(t *testing.T) {
	// Zero value = active; JSON omits the fields so existing emitters and
	// fixtures are byte-compatible.
	pl := channelevents.PlanUpdatePayload{
		PlanName: "main",
		Items:    []channelevents.PlanItemRef{{ID: "a", Label: "A", Status: "pending"}},
	}
	require.False(t, pl.Paused)
	require.Empty(t, pl.PauseCause)

	b, err := json.Marshal(pl)
	require.NoError(t, err)
	require.NotContains(t, string(b), "paused")
	require.NotContains(t, string(b), "pauseCause")

	// Validate is unaffected by the new fields.
	require.NoError(t, pl.Validate())
}

func TestPlanUpdatePayload_PausedRoundTrips(t *testing.T) {
	pl := channelevents.PlanUpdatePayload{
		PlanName:   "main",
		Items:      []channelevents.PlanItemRef{{ID: "a", Label: "A", Status: "in_progress"}},
		Paused:     true,
		PauseCause: channelevents.PauseCauseApproval,
	}
	b, err := json.Marshal(pl)
	require.NoError(t, err)

	var got channelevents.PlanUpdatePayload
	require.NoError(t, json.Unmarshal(b, &got))
	require.True(t, got.Paused)
	require.Equal(t, "awaiting_approval", got.PauseCause)
}
