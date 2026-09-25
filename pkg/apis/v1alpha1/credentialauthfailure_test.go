package v1alpha1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestUpsertCredentialAuthFailure(t *testing.T) {
	t.Run("appends an origin not yet present", func(t *testing.T) {
		got := UpsertCredentialAuthFailure(nil, CredentialAuthFailure{Origin: "mcpserver/one", Count: 1})
		require.Len(t, got, 1)
		assert.Equal(t, "mcpserver/one", got[0].Origin)
	})

	t.Run("replaces in place, never duplicating an origin", func(t *testing.T) {
		list := []CredentialAuthFailure{
			{Origin: "mcpserver/one", Count: 1},
			{Origin: "mcpserver/two", Count: 1},
		}
		got := UpsertCredentialAuthFailure(list, CredentialAuthFailure{Origin: "mcpserver/one", Count: 4})
		require.Len(t, got, 2, "an origin already present must be replaced, not appended again")
		assert.Equal(t, int32(4), got[0].Count)
		assert.Equal(t, "mcpserver/two", got[1].Origin, "sibling entries are untouched")
	})
}

func TestRemoveCredentialAuthFailure(t *testing.T) {
	t.Run("drops only the named origin", func(t *testing.T) {
		list := []CredentialAuthFailure{
			{Origin: "mcpserver/one"},
			{Origin: "mcpserver/two"},
			{Origin: "mcpserver/three"},
		}
		got := RemoveCredentialAuthFailure(list, "mcpserver/two")
		require.Len(t, got, 2)
		assert.Equal(t, "mcpserver/one", got[0].Origin)
		assert.Equal(t, "mcpserver/three", got[1].Origin)
	})

	t.Run("removing an absent origin is a no-op", func(t *testing.T) {
		list := []CredentialAuthFailure{{Origin: "mcpserver/one"}}
		got := RemoveCredentialAuthFailure(list, "mcpserver/absent")
		require.Len(t, got, 1)
		assert.Equal(t, "mcpserver/one", got[0].Origin)
	})

	t.Run("removing the last entry yields nil, so the merge patch DELETES the field", func(t *testing.T) {
		got := RemoveCredentialAuthFailure([]CredentialAuthFailure{{Origin: "mcpserver/one"}}, "mcpserver/one")
		assert.Nil(t, got)

		// The clear-on-success write is a JSON merge patch computed by
		// marshaling the mutated status. An empty-but-non-nil slice would
		// marshal to `[]` and, with omitempty, still be dropped — but nil makes
		// the intent explicit and matches the other runner-owned lists.
		b, err := json.Marshal(AgentSessionStatus{CredentialAuthFailures: got})
		require.NoError(t, err)
		assert.NotContains(t, string(b), "credentialAuthFailures",
			"the cleared field must be absent from the marshaled status, or a stale observation survives the success that retracted it")
	})
}

func TestFindCredentialAuthFailure(t *testing.T) {
	list := []CredentialAuthFailure{
		{Origin: "mcpserver/one", Count: 1},
		{Origin: "toolkit/two", Count: 5},
	}

	t.Run("returns the entry for a present origin", func(t *testing.T) {
		got := FindCredentialAuthFailure(list, "toolkit/two")
		require.NotNil(t, got, "presence is the corroboration signal; a miss here silently disables the unverified tier")
		assert.Equal(t, "toolkit/two", got.Origin)
		assert.Equal(t, int32(5), got.Count)
	})

	t.Run("returns nil for an origin the session never observed", func(t *testing.T) {
		assert.Nil(t, FindCredentialAuthFailure(list, "mcpserver/absent"),
			"one origin's dead credential must not corroborate a request about another")
	})

	t.Run("returns nil for an empty list", func(t *testing.T) {
		assert.Nil(t, FindCredentialAuthFailure(nil, "mcpserver/one"))
	})

	t.Run("an empty origin never matches, even against a malformed entry", func(t *testing.T) {
		// The runner refuses to record an empty origin, so this list is not
		// something it can produce -- which is exactly why the guard is here:
		// a request with no origin must fail closed rather than be corroborated
		// by whatever an unowned writer left behind.
		assert.Nil(t, FindCredentialAuthFailure([]CredentialAuthFailure{{Origin: ""}}, ""))
	})
}

func TestCredentialAuthFailureRoundTripsThroughJSON(t *testing.T) {
	now := metav1.Now()
	in := AgentSessionStatus{CredentialAuthFailures: []CredentialAuthFailure{
		{Origin: "mcpserver/demo", Count: 3, ObservedAt: &now},
	}}
	b, err := json.Marshal(in)
	require.NoError(t, err)

	var out AgentSessionStatus
	require.NoError(t, json.Unmarshal(b, &out))
	require.Len(t, out.CredentialAuthFailures, 1)
	assert.Equal(t, "mcpserver/demo", out.CredentialAuthFailures[0].Origin)
	assert.Equal(t, int32(3), out.CredentialAuthFailures[0].Count)
	require.NotNil(t, out.CredentialAuthFailures[0].ObservedAt)
}
