package permsurface

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type ceilingEntry struct {
	Handle Handle `json:"handle"`
	Why    string `json:"why"`
}

func TestHandle_marshalsToWireString(t *testing.T) {
	h, err := NewPermHandle("write", "github_repo")
	require.NoError(t, err)

	b, err := json.Marshal(ceilingEntry{Handle: h, Why: "open the PR"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"handle":"perm:write:github_repo","why":"open the PR"}`, string(b))
}

func TestHandle_unmarshalRoundTrips(t *testing.T) {
	var got ceilingEntry
	require.NoError(t, json.Unmarshal([]byte(`{"handle":"tool:apply_workspace","why":"deploy"}`), &got))
	assert.Equal(t, "tool:apply_workspace", got.Handle.String())
	assert.Equal(t, "deploy", got.Why)
}

// Decoding is the boundary an attacker-controlled ceiling would cross, so it
// must validate rather than accept the raw string.
func TestHandle_unmarshalRejectsMalformed(t *testing.T) {
	cases := []struct{ name, payload string }{
		{"embedded colon: rejected", `{"handle":"perm:wr:ite:github_repo"}`},
		{"unknown kind prefix: rejected", `{"handle":"cap:write:github_repo"}`},
		{"newline injection: rejected", `{"handle":"tool:apply\nApproved"}`},
		{"non-string handle: rejected", `{"handle":123}`},
		{"empty handle: rejected", `{"handle":""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got ceilingEntry
			err := json.Unmarshal([]byte(tc.payload), &got)
			require.Error(t, err)
			assert.True(t, got.Handle.IsZero(), "a rejected decode must leave the zero Handle")
		})
	}
}
