package workspace_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/platform/workspace"
)

func TestSnapshotHandle_PathSegment(t *testing.T) {
	h := workspace.SnapshotHandle{
		SessionUID: "11111111-2222",
		TurnIndex:  7,
		Sequence:   2,
	}
	assert.Equal(t, "11111111-2222/000007-002", h.PathSegment())
}

func TestPVCRef_String(t *testing.T) {
	p := workspace.PVCRef{Namespace: "ns", Name: "sess-workspace"}
	assert.Equal(t, "ns/sess-workspace", p.String())
}
