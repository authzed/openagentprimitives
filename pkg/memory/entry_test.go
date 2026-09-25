package memory_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

func TestEntry_JSONRoundtrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	e := memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: "ns/name"},
		Kind:      "authz_decision",
		ID:        "authzd-abc",
		CreatedAt: now,
		Links: []memory.Link{{
			Relation: "for_resource", Kind: "github_repo", ID: "authzed/spicedb",
		}},
		Tags:    []string{"outcome:denied"},
		Content: json.RawMessage(`{"x":1}`),
	}

	raw, err := json.Marshal(e)
	require.NoError(t, err)

	var got memory.Entry
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, e, got)
}

func TestScopeStatus_States(t *testing.T) {
	assert.Equal(t, "live", string(memory.StatusLive))
	assert.Equal(t, "archived", string(memory.StatusArchived))
	assert.Equal(t, "unknown", string(memory.StatusUnknown))
}

func TestSignal_JSONRoundtrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	s := memory.Signal{
		Kind:    memory.SignalKind("lifecycle/session.started"),
		Scope:   memory.Scope{Kind: "session", ID: "ns/name"},
		At:      now,
		Payload: json.RawMessage(`{"agentclass":"hubspot"}`),
	}
	raw, err := json.Marshal(s)
	require.NoError(t, err)

	var got memory.Signal
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, s, got)
}
