package admind_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/web/admind/audit"
)

// auditApproval seeds one approval audit entry scoped to a "ns/name" session,
// through the same facade the audit engine queries. The entry id is derived
// from the scope so multiple sessions don't collide.
func auditApproval(t *testing.T, scopeID string) memory.Entry {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"toolName": "bash", "decision": "approved", "approver": "user:abc"})
	require.NoError(t, err)
	return memory.Entry{
		Scope:   memory.Scope{Kind: "session", ID: scopeID},
		Kind:    "approval",
		ID:      "approval-" + strings.ReplaceAll(scopeID, "/", "-"),
		Content: raw,
	}
}

func entityRowsByKey(rows []audit.EntityRow) map[string]audit.EntityRow {
	out := make(map[string]audit.EntityRow, len(rows))
	for _, r := range rows {
		out[r.Key] = r
	}
	return out
}

// TestAdmindAuditEntities_SessionsEnriched proves the sessions rollup joins
// each row against the live aggregator (status + tokens + est cost) and the
// started-by annotation, while a GC'd session (audit-only) stays a bare row.
func TestAdmindAuditEntities_SessionsEnriched(t *testing.T) {
	// s1 is live (tracked, started by alice); s2 exists only in the audit log
	// (no live state, no AgentSession CR → GC'd, no starter).
	s1 := budgetSession("default", "s1", "support-bot", "anthropic", "claude-opus-4-8", "user:alice", 1000, 200)
	s1.Status.Phase = "Running"

	k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(s1, priceCatalogSettings()).Build()
	a := newTestAdmind(t, k8s)
	a.Aggregator().UpsertSession(s1) // only s1 is live

	ctx := context.Background()
	for _, id := range []string{"default/s1", "default/s2"} {
		_, err := a.Memory().Put(ctx, auditApproval(t, id))
		require.NoError(t, err)
	}
	h := a.Handler()

	w := do(t, h, http.MethodGet, "/admin/v1/audit/entities/sessions", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)
	var rows []audit.EntityRow
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rows))
	byKey := entityRowsByKey(rows)

	// opus 15/75 USD per MTok (priceCatalogSettings' modelCatalog overrides the
	// built-in base) → est cost for s1's 1000 in / 200 out.
	const (
		costS1 = 1000.0/1e6*15 + 200.0/1e6*75 // 0.0300
		eps    = 1e-9
	)

	// s1: tracked + started → status/tokens/estCost/startedBy/startedAt all populated.
	s1row, ok := byKey["default/s1"]
	require.True(t, ok, "s1 row present")
	assert.Equal(t, 1, s1row.Events)
	assert.Equal(t, "Running", s1row.Status)
	assert.Equal(t, int64(1000), s1row.InputTokens)
	assert.Equal(t, int64(200), s1row.OutputTokens)
	assert.InDelta(t, costS1, float64(s1row.EstimatedCostUSD), eps)
	assert.Equal(t, "user:alice", s1row.StartedBy)
	// startedAt joins from the AgentSession's status.startedAt (budgetSession
	// stamps 2026-06-01) — the Sessions audit table can show a Started date.
	require.NotNil(t, s1row.StartedAt, "live session's startedAt is populated")
	assert.Equal(t, 2026, s1row.StartedAt.Year())
	assert.Equal(t, time.June, s1row.StartedAt.Month())

	// s2: audit-only (GC'd out of the live aggregator) → bare events row, no
	// status/tokens/cost/starter. Audit outlives sessions; no error.
	s2row, ok := byKey["default/s2"]
	require.True(t, ok, "s2 row present (audit outlives the live session)")
	assert.Equal(t, 1, s2row.Events)
	assert.Empty(t, s2row.Status, "GC'd session carries no live status")
	assert.Zero(t, s2row.InputTokens)
	assert.Zero(t, s2row.OutputTokens)
	assert.Zero(t, s2row.EstimatedCostUSD)
	assert.Empty(t, s2row.StartedBy)
	assert.Nil(t, s2row.StartedAt, "GC'd session (no AgentSession CR) carries no startedAt")
}

// TestAdmindAuditEntities_NonSessionAxesPlain proves the enrichment is
// sessions-axis-only: the agents/tools/users rollups never carry the
// session-only fields (they stay omitted via omitempty).
func TestAdmindAuditEntities_NonSessionAxesPlain(t *testing.T) {
	s1 := budgetSession("default", "s1", "support-bot", "anthropic", "claude-opus-4-8", "user:alice", 1000, 200)
	s1.Status.Phase = "Running"
	k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(s1).Build()
	a := newTestAdmind(t, k8s)
	a.Aggregator().UpsertSession(s1)

	_, err := a.Memory().Put(context.Background(), auditApproval(t, "default/s1"))
	require.NoError(t, err)
	h := a.Handler()

	for _, axis := range []string{"agents", "tools", "users"} {
		t.Run(axis+": no session-only fields", func(t *testing.T) {
			w := do(t, h, http.MethodGet, "/admin/v1/audit/entities/"+axis, "test-token", "user:YWRtaW4", "")
			require.Equal(t, http.StatusOK, w.Code)
			body := w.Body.String()
			for _, field := range []string{"status", "inputTokens", "outputTokens", "estimatedCostUSD", "startedBy", "startedAt"} {
				assert.NotContains(t, body, "\""+field+"\"",
					axis+" rows must omit the session-only field "+field)
			}
			// Sanity: the plain shape is still present.
			var rows []audit.EntityRow
			require.NoError(t, json.Unmarshal([]byte(body), &rows))
			require.NotEmpty(t, rows)
		})
	}
}
