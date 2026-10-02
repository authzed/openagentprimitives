package plangateaudit

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

func TestKind_isAppendOnly(t *testing.T) {
	assert.True(t, Kind{}.Retention().AppendOnly,
		"plan-gate records are authorization evidence — write-once")
}

func TestKind_nameAndPrefix(t *testing.T) {
	assert.Equal(t, "plan_gate_audit", Kind{}.Name())
	assert.Equal(t, "pgaud-", Kind{}.IDPrefix())
}

func TestKind_archivesOnSessionTerminal(t *testing.T) {
	got := Kind{}.Retention().ArchiveOn
	assert.Contains(t, got, lifecycle.SigSessionCompleted)
	assert.Contains(t, got, lifecycle.SigSessionFailed)
}

// The fold replays by Event and the logging-mode dataset queries by Outcome, so
// both must be indexed or every query degrades to a full scan.
func TestKind_indexesTheFieldsTheFoldAndDatasetQuery(t *testing.T) {
	got := Kind{}.IndexedFields()
	for _, want := range []string{"at", "tool", "event", "outcome"} {
		assert.Contains(t, got, want)
	}
}

// PhaseIndex is a POINTER because phase 0 is the common case and
// must not be indistinguishable from "no phase recorded".
func TestContent_phaseZeroSurvivesRoundTrip(t *testing.T) {
	zero := int32(0)
	in := Content{Event: EventPhaseSelected, PhaseIndex: &zero}

	b, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"phaseIndex":0`,
		"phase 0 must serialize, not be omitted as empty")

	var out Content
	require.NoError(t, json.Unmarshal(b, &out))
	require.NotNil(t, out.PhaseIndex)
	assert.Equal(t, int32(0), *out.PhaseIndex)
}

func TestContent_unsetPhaseIndexIsOmitted(t *testing.T) {
	b, err := json.Marshal(Content{Event: EventPlanApproved})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "phaseIndex",
		"an unset phase must be absent, not serialized as 0")
}

// Mode has no omitempty: a record that does not say which mode produced it
// cannot be interpreted, because "would_deny" means something entirely
// different under logging than under enforcing.
func TestContent_modeIsAlwaysSerialized(t *testing.T) {
	b, err := json.Marshal(Content{Event: EventGateAllowed})
	require.NoError(t, err)
	assert.Contains(t, string(b), `"mode"`)
}

func TestContent_roundTripsEveryField(t *testing.T) {
	idx := int32(2)
	in := Content{
		Event:      EventGateWouldDeny,
		PlanDigest: "sha256:abc",
		PhaseIndex: &idx,
		Handle:     "perm:write:tracker_issue",
		Tool:       "update_issue",
		UseID:      "toolu_01",
		Outcome:    OutcomeWouldDeny,
		Severity:   "elevated",
		Tier:       "1",
		CardJSON:   `{"what":"..."}`,
		Mode:       "logging",
		Provenance: "plan_gate",
	}

	b, err := json.Marshal(in)
	require.NoError(t, err)

	var out Content
	require.NoError(t, json.Unmarshal(b, &out))

	require.NotNil(t, out.PhaseIndex)
	assert.Equal(t, idx, *out.PhaseIndex)
	out.PhaseIndex, in.PhaseIndex = nil, nil
	assert.Equal(t, in, out)
}

// SlotRef.Standing is ADDITIVE: this log is append-only, so a record written
// before the field existed must still unmarshal and still fold. Absent must
// read as "not recorded", not as an unmarshal failure.
func TestContent_StandingIsAdditive(t *testing.T) {
	const legacy = `{"event":"phase_approved","phaseKey":"k1","slotRefs":[{"type":"git_repo","id":"acme/app"}]}`
	var c Content
	require.NoError(t, json.Unmarshal([]byte(legacy), &c))
	require.Len(t, c.SlotRefs, 1)
	assert.Equal(t, "", c.SlotRefs[0].Standing, "absent, not invalid")
}

// A record written WITH the field round-trips it like any other.
func TestContent_StandingRoundTrips(t *testing.T) {
	in := Content{
		Event:    EventPhaseApproved,
		SlotRefs: []SlotRef{{Type: "git_repo", ID: "acme/app", Standing: "session-only"}},
	}
	b, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"standing":"session-only"`)

	var out Content
	require.NoError(t, json.Unmarshal(b, &out))
	require.Len(t, out.SlotRefs, 1)
	assert.Equal(t, "session-only", out.SlotRefs[0].Standing)
}

// SlotRef.MovedFrom is ADDITIVE for the same append-only reason as Standing: a
// record written before the field existed must still unmarshal, with it empty
// (read everywhere downstream as "no move").
func TestContent_MovedFromIsAdditive(t *testing.T) {
	const legacy = `{"event":"phase_approved","phaseKey":"k1","slotRefs":[{"type":"git_repo","id":"acme/app"}]}`
	var c Content
	require.NoError(t, json.Unmarshal([]byte(legacy), &c))
	require.Len(t, c.SlotRefs, 1)
	assert.Equal(t, "", c.SlotRefs[0].MovedFrom, "absent, not invalid")
}

// A record written WITH MovedFrom round-trips it, and it is omitted when empty
// so a first-fill record is byte-identical to one written before the field.
func TestContent_MovedFromRoundTripsAndOmitsWhenEmpty(t *testing.T) {
	in := Content{
		Event:    EventPhaseApproved,
		SlotRefs: []SlotRef{{Type: "git_repo", ID: "acme/new", MovedFrom: "acme/old"}},
	}
	b, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"movedFrom":"acme/old"`)

	var out Content
	require.NoError(t, json.Unmarshal(b, &out))
	require.Len(t, out.SlotRefs, 1)
	assert.Equal(t, "acme/old", out.SlotRefs[0].MovedFrom)

	firstFill, err := json.Marshal(Content{SlotRefs: []SlotRef{{Type: "git_repo", ID: "acme/new"}}})
	require.NoError(t, err)
	assert.NotContains(t, string(firstFill), "movedFrom", "a first-fill records no move")
}

// NOTE: registration is NOT asserted here. This package's init() registers the
// kind in its own test binary whether or not kinds/all imports it, so such a
// test would assert nothing. The real assertion lives in pkg/memory/kinds/all.
