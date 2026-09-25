package runner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakageaudit"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// The per-datum egress audit kinds must PERSIST, not be logged-and-dropped —
// they are the tag-coverage telemetry the feature relies on to measure precision.
func TestWriteAudit_persistsFineGrainedKinds(t *testing.T) {
	var got []infoleakageaudit.AuditRecord
	l := &Loop{AuditMemoryAppend: func(_ context.Context, rec infoleakageaudit.AuditRecord) error {
		got = append(got, rec)
		return nil
	}}
	h := newRunnerHost(l, hostSession{Namespace: "ns", Name: "s"})

	kinds := []string{
		"tool_call_no_leak_per_datum", "tool_call_leak_per_datum",
		"tool_call_destination_unresolved", "respond_no_leak_per_datum", "respond_leak_per_datum",
	}
	for _, k := range kinds {
		require.NoError(t, h.Audit(context.Background(), []pipeline.AuditRecord{{Kind: k, Fields: map[string]any{"tool": "t"}}}))
	}

	require.Len(t, got, len(kinds), "every fine-grained kind is persisted, not logged-and-dropped")
	for _, rec := range got {
		assert.Equal(t, "ns/s", rec.Session)
	}
}
