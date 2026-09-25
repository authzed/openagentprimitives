package runner_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/agent/runner"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

func TestPreDispatchSnapshotForImpact(t *testing.T) {
	sessUID := "uid-1"
	cases := []struct {
		name   string
		impact authz.StateImpact
		seq    int32
		turn   int32
		want   *spiceboxv1alpha1.PreDispatchSnapshot
	}{
		{"stateless", authz.Stateless, 0, 1, nil},
		{"passthrough", authz.Passthrough, 0, 1, nil},
		{"readonly", authz.Readonly, 0, 1, nil},
		{"readwrite", authz.Readwrite, 0, 5, &spiceboxv1alpha1.PreDispatchSnapshot{SessionUID: sessUID, TurnIndex: 5, Sequence: 0}},
		{"external", authz.External, 2, 5, &spiceboxv1alpha1.PreDispatchSnapshot{SessionUID: sessUID, TurnIndex: 5, Sequence: 2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runner.PreDispatchSnapshotForImpact(sessUID, tc.turn, tc.seq, tc.impact)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestPreDispatchSnapshotForImpact_EmptyUID(t *testing.T) {
	// Defensive: an empty UID means we don't know the parent session
	// (kubectl-driven session without UID propagation, tests, etc.).
	// Returning nil avoids creating snapshot handles that can't be
	// resolved later.
	got := runner.PreDispatchSnapshotForImpact("", 5, 0, authz.Readwrite)
	assert.Nil(t, got, "empty sessionUID disables snapshotting")
}
