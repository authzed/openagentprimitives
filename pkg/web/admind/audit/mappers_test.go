package audit_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/web/admind/audit"
)

func entry(kind, id string, content any) memory.Entry {
	raw, _ := json.Marshal(content)
	return memory.Entry{
		Scope: memory.Scope{Kind: "session", ID: "default/s1"},
		Kind:  kind, ID: id,
		CreatedAt: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
		Content:   raw,
	}
}

func TestMappers(t *testing.T) {
	cases := []struct {
		name  string
		entry memory.Entry
		check func(t *testing.T, evs []audit.Event)
	}{
		{
			name:  "approval request → kind=approval outcome=pending",
			entry: entry("approval", "approval-1", map[string]any{"toolName": "terraform_apply", "approver": "user:abc"}),
			check: func(t *testing.T, evs []audit.Event) {
				require.Len(t, evs, 1)
				assert.Equal(t, "approval", evs[0].Kind)
				assert.Equal(t, "pending", evs[0].Outcome)
				assert.Equal(t, "terraform_apply", evs[0].Tool)
				assert.Equal(t, "default", evs[0].SessionNamespace)
				assert.Equal(t, "s1", evs[0].SessionName)
			},
		},
		{
			name:  "approval outcome → outcome=denied actor=approver",
			entry: entry("approval", "approval-2", map[string]any{"toolName": "terraform_apply", "decision": "denied", "approver": "user:abc"}),
			check: func(t *testing.T, evs []audit.Event) {
				require.Len(t, evs, 1)
				assert.Equal(t, "denied", evs[0].Outcome)
				assert.Equal(t, "user:abc", evs[0].Actor)
			},
		},
		{
			name:  "authz_decision → kind=authz_decision outcome from content",
			entry: entry("authz_decision", "authzd-1", map[string]any{"outcome": "denied", "subject": "user:abc", "resourceType": "repo", "resourceId": "r1", "permission": "push"}),
			check: func(t *testing.T, evs []audit.Event) {
				require.Len(t, evs, 1)
				assert.Equal(t, "authz_decision", evs[0].Kind)
				assert.Equal(t, "denied", evs[0].Outcome)
				assert.Contains(t, evs[0].Summary, "push")
			},
		},
		{
			name:  "scope_audit → kind=scope_change",
			entry: entry("scope_audit", "saud-1", map[string]any{"source": "tool_grant", "approver": "user:abc", "appliedAt": "2026-06-01T12:30:00Z"}),
			check: func(t *testing.T, evs []audit.Event) {
				require.Len(t, evs, 1)
				assert.Equal(t, "scope_change", evs[0].Kind)
				assert.Equal(t, "applied", evs[0].Outcome)
				assert.Equal(t, 30, evs[0].Time.Minute(), "AppliedAt overrides CreatedAt")
			},
		},
		{
			name:  "infoleakage_audit → kind=leakage outcome from record subtype",
			entry: entry("infoleakage_audit", "ila-1", map[string]any{"kind": "read_denied", "tool": "fetch", "requester": "user:abc", "at": "2026-06-01T12:05:00Z"}),
			check: func(t *testing.T, evs []audit.Event) {
				require.Len(t, evs, 1)
				assert.Equal(t, "leakage", evs[0].Kind)
				assert.Equal(t, "read_denied", evs[0].Outcome)
				assert.Equal(t, "fetch", evs[0].Tool)
			},
		},
		{
			name:  "relwrites_audit → tuple count in summary",
			entry: entry("relwrites_audit", "relw-1", map[string]any{"source": "tool:gh", "tuples": []any{map[string]any{"resource": "a", "relation": "b", "subject": "c"}}}),
			check: func(t *testing.T, evs []audit.Event) {
				require.Len(t, evs, 1)
				assert.Equal(t, "rel_writes", evs[0].Kind)
				assert.Contains(t, evs[0].Summary, "1 tuple")
			},
		},
		{
			name: "lifecycle → summary from signal tag",
			entry: func() memory.Entry {
				e := entry("lifecycle", "lifecycle-turn-completed-x", map[string]any{})
				e.Tags = []string{"lifecycle/turn.completed"}
				return e
			}(),
			check: func(t *testing.T, evs []audit.Event) {
				require.Len(t, evs, 1)
				assert.Equal(t, "lifecycle", evs[0].Kind)
				assert.Equal(t, "lifecycle/turn.completed", evs[0].Summary)
			},
		},
		{
			name:  "tool_session → info summary from raw content",
			entry: entry("tool_session", "ts-1", map[string]any{"event": "sandbox_open", "tool": "bash"}),
			check: func(t *testing.T, evs []audit.Event) {
				require.Len(t, evs, 1)
				assert.Equal(t, "tool_session", evs[0].Kind)
				assert.Equal(t, "info", evs[0].Outcome)
				assert.NotEmpty(t, evs[0].Summary)
			},
		},
		{
			name: "turn text-only → 0 events (nil,nil contract)",
			entry: entry("turn", "turn-text-only", map[string]any{
				"index": 1, "role": "assistant",
				"content": []any{
					map[string]any{"type": "text", "text": "thinking"},
				},
			}),
			check: func(t *testing.T, evs []audit.Event) {
				require.Empty(t, evs)
			},
		},
		{
			name: "turn with tool_use blocks → one tool_call event per block; none for plain text",
			entry: entry("turn", "turn-3-assistant", map[string]any{
				"index": 3, "role": "assistant",
				"content": []any{
					map[string]any{"type": "text", "text": "thinking"},
					map[string]any{"type": "tool_use", "toolUse": map[string]any{"id": "tu1", "name": "bash", "input": []byte(`{}`)}},
				},
			}),
			check: func(t *testing.T, evs []audit.Event) {
				require.Len(t, evs, 1)
				assert.Equal(t, "tool_call", evs[0].Kind)
				assert.Equal(t, "bash", evs[0].Tool)
				assert.Equal(t, "dispatched", evs[0].Outcome)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, ok := audit.MapperFor(tc.entry.Kind)
			require.True(t, ok, "mapper registered for %q", tc.entry.Kind)
			evs, err := m.Map(tc.entry)
			require.NoError(t, err)
			tc.check(t, evs)
		})
	}
}

func TestRegistryCoversAllMemoryKinds(t *testing.T) {
	want := []string{"approval", "authz_decision", "infoleakage_audit", "lifecycle",
		"relwrites_audit", "scope_audit", "tool_session", "turn"}
	assert.ElementsMatch(t, want, audit.MemoryKinds())
}
