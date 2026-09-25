package channelevents

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKindValid(t *testing.T) {
	cases := []struct {
		name string
		in   Kind
		want bool
	}{
		{"user_message: valid", KindUserMessage, true},
		{"permission_request: valid", KindPermissionRequest, true},
		{"permission_grant: valid", KindPermissionGrant, true},
		{"notification: valid", KindNotification, true},
		{"tool_activity: valid", KindToolActivity, true},
		{"empty: invalid", Kind(""), false},
		{"versioned suffix: invalid", Kind("user_message_v2"), false},
		{"uppercase: invalid", Kind("USER_MESSAGE"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.in.Valid())
		})
	}
}

func TestKindAllImplemented(t *testing.T) {
	cases := []struct {
		name string
		kind Kind
		want bool
	}{
		{"user_message: implemented", KindUserMessage, true},
		{"notification: implemented (status updates)", KindNotification, true},
		{"tool_activity: implemented (watchdog tick)", KindToolActivity, true},
		// permission_request and permission_decision_applied are now wired
		// (T16–T18 plan). permission_grant remains reserved.
		{"permission_request: implemented (T16-T18)", KindPermissionRequest, true},
		{"permission_decision: implemented (T17)", KindPermissionDecision, true},
		{"permission_decision_applied: implemented (T18)", KindPermissionDecisionApplied, true},
		{"permission_grant: reserved, NOT implemented", KindPermissionGrant, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.kind.Implemented())
		})
	}
}

func TestKindLiveViewOffer_ValidAndImplemented(t *testing.T) {
	assert.True(t, KindLiveViewOffer.Valid())
	assert.True(t, KindLiveViewOffer.Implemented())
	assert.Equal(t, "live_view_offer", string(KindLiveViewOffer))
}

func TestKindThreadTitle_ValidAndImplemented(t *testing.T) {
	assert.True(t, KindThreadTitle.Valid(), "thread_title must be Valid")
	assert.True(t, KindThreadTitle.Implemented(), "thread_title must be Implemented")
	assert.Equal(t, Kind("thread_title"), KindThreadTitle)
}

func TestKindSessionViewOffer_ValidAndImplemented(t *testing.T) {
	assert.True(t, KindSessionViewOffer.Valid(), "session_view_offer must be Valid")
	assert.True(t, KindSessionViewOffer.Implemented(), "session_view_offer must be Implemented")
	assert.Equal(t, Kind("session_view_offer"), KindSessionViewOffer)
}

func TestKindWidgetOffer_ValidAndImplemented(t *testing.T) {
	assert.True(t, KindWidgetOffer.Valid(), "widget_offer must be Valid")
	assert.True(t, KindWidgetOffer.Implemented(), "widget_offer must be Implemented")
	assert.Equal(t, Kind("widget_offer"), KindWidgetOffer)
}

// Each permission payload has a distinct concrete type, so they don't
// share a clean table shape; sibling subtests with the same arc
// (marshal → unmarshal → equality) keep the symmetry without forcing
// `any` indirection.
func TestPermissionPayloads_JSONRoundtrip(t *testing.T) {
	t.Run("PermissionRequestPayload: roundtrips", func(t *testing.T) {
		in := PermissionRequestPayload{
			AgentSessionRef: SessionRef{Namespace: "default", Name: "sess-1"},
			Requester:       ExternalIdentity{Kind: "slack", ExternalID: "U_SAM"},
			StartedBy:       ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE"},
			Preview:         "hey can I see…",
		}
		data, err := json.Marshal(in)
		require.NoError(t, err, "marshal")
		var got PermissionRequestPayload
		require.NoError(t, json.Unmarshal(data, &got), "unmarshal")
		assert.Equal(t, in, got)
	})

	t.Run("PermissionDecisionPayload with RequestRef: roundtrips", func(t *testing.T) {
		in := PermissionDecisionPayload{
			AgentSessionRef: SessionRef{Namespace: "default", Name: "sess-1"},
			Requester:       ExternalIdentity{Kind: "slack", ExternalID: "U_SAM"},
			Approver:        ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE"},
			Decision:        "approve",
			RequestRef:      "opaque-blob",
		}
		data, err := json.Marshal(in)
		require.NoError(t, err, "marshal")
		var got PermissionDecisionPayload
		require.NoError(t, json.Unmarshal(data, &got), "unmarshal")
		assert.Equal(t, in, got)
	})

	t.Run("PermissionDecisionAppliedPayload: roundtrips", func(t *testing.T) {
		in := PermissionDecisionAppliedPayload{
			AgentSessionRef: SessionRef{Namespace: "default", Name: "sess-1"},
			Requester:       ExternalIdentity{Kind: "slack", ExternalID: "U_SAM"},
			Approver:        ExternalIdentity{Kind: "slack", ExternalID: "U_ALICE"},
			Decision:        "deny",
			RequestRef:      "opaque-blob",
		}
		data, err := json.Marshal(in)
		require.NoError(t, err, "marshal")
		var got PermissionDecisionAppliedPayload
		require.NoError(t, json.Unmarshal(data, &got), "unmarshal")
		assert.Equal(t, in, got)
	})
}
