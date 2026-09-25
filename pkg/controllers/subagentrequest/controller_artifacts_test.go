package subagentrequest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestReconcile_ChildSucceeded_CarriesReturnedArtifactsToTheRequest is the
// propagation itself: without it a child's return_result accepts artifact
// handles, writes them to its own status, and nothing ever reads them — the
// parent cannot see that an artifact exists, let alone deliver it.
func TestReconcile_ChildSucceeded_CarriesReturnedArtifactsToTheRequest(t *testing.T) {
	sr := requestWithChild(t, "reqa1", "demo-parent", "demo-coder", "reqa1-child")
	child := childSession(t, "reqa1-child", "demo-coder", v1.AgentSessionStatus{
		Phase: v1.AgentSessionPhaseSucceeded,
		Result: &v1.AgentResult{
			Summary: "the report is attached",
			Artifacts: []v1.ResultArtifact{
				{ID: "ar-reqa1-child-aaa", Description: "the findings report"},
				{ID: "ar-reqa1-child-bbb", Description: "the raw data"},
			},
		},
	})
	r, c := newReconciler(t, sr, child)

	reconcileOnce(t, r, "reqa1")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "reqa1"}, &got))
	require.Equal(t, v1.SubagentRequestPhaseSucceeded, got.Status.Phase)
	require.Len(t, got.Status.Artifacts, 2, "every handle the child returned must reach the request")
	assert.Equal(t, "ar-reqa1-child-aaa", got.Status.Artifacts[0].ID)
	assert.Equal(t, "the findings report", got.Status.Artifacts[0].Description)
	assert.Equal(t, "ar-reqa1-child-bbb", got.Status.Artifacts[1].ID)
	assert.Equal(t, "the raw data", got.Status.Artifacts[1].Description)
}

// TestReconcile_ChildSucceeded_NoArtifactsLeavesTheListEmpty pins the ordinary
// case: a child that returned only words must not grow an empty-handle entry
// the parent's model could then be told to attach.
func TestReconcile_ChildSucceeded_NoArtifactsLeavesTheListEmpty(t *testing.T) {
	sr := requestWithChild(t, "reqa2", "demo-parent", "demo-coder", "reqa2-child")
	child := childSession(t, "reqa2-child", "demo-coder", v1.AgentSessionStatus{
		Phase:  v1.AgentSessionPhaseSucceeded,
		Result: &v1.AgentResult{Summary: "no files, just the answer"},
	})
	r, c := newReconciler(t, sr, child)

	reconcileOnce(t, r, "reqa2")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "reqa2"}, &got))
	assert.Empty(t, got.Status.Artifacts)
	assert.Equal(t, "child reqa2-child succeeded", got.Status.Determination,
		"nothing was cut, so the determination must not claim anything was")
}

// TestReconcile_ChildSucceeded_OverflowingArtifactListIsCutAndSaidSo is the
// no-silent-drop half of the bound. The parent is about to be shown the
// handles that survived and has no way to tell a child that returned three
// from one that returned far more, so the cut has to be visible to whoever
// reads the request.
func TestReconcile_ChildSucceeded_OverflowingArtifactListIsCutAndSaidSo(t *testing.T) {
	returned := make([]v1.ResultArtifact, 0, maxCopiedArtifacts+3)
	for i := range maxCopiedArtifacts + 3 {
		returned = append(returned, v1.ResultArtifact{
			ID:          fmt.Sprintf("ar-reqa3-child-%02d", i),
			Description: "one of many",
		})
	}
	sr := requestWithChild(t, "reqa3", "demo-parent", "demo-coder", "reqa3-child")
	child := childSession(t, "reqa3-child", "demo-coder", v1.AgentSessionStatus{
		Phase:  v1.AgentSessionPhaseSucceeded,
		Result: &v1.AgentResult{Summary: "lots of files", Artifacts: returned},
	})
	r, c := newReconciler(t, sr, child)

	reconcileOnce(t, r, "reqa3")

	var got v1.SubagentRequest
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "reqa3"}, &got))
	assert.Len(t, got.Status.Artifacts, maxCopiedArtifacts, "the list must be cut to the bound")
	assert.Contains(t, got.Status.Determination, "dropped",
		"a cut list must say it was cut; a silent one reads as everything the child returned")
	assert.Contains(t, got.Status.Determination, "succeeded",
		"the cut is an addition to the determination, not a replacement for the outcome")
}

func TestCopyReturnedArtifacts_PerEntryBounds(t *testing.T) {
	longDesc := strings.Repeat("d", maxCopiedArtifactDescLen*2)
	longID := "ar-" + strings.Repeat("x", maxCopiedArtifactIDLen)

	cases := []struct {
		name        string
		in          []v1.ResultArtifact
		wantIDs     []string
		wantDropped int
		check       func(t *testing.T, out []v1.ResultArtifact)
	}{
		{
			name:    "handles within every bound: carried verbatim, nothing dropped",
			in:      []v1.ResultArtifact{{ID: "ar-a", Description: "fine"}},
			wantIDs: []string{"ar-a"},
		},
		{
			name:    "over-long description: entry kept, description cut with the marker",
			in:      []v1.ResultArtifact{{ID: "ar-a", Description: longDesc}},
			wantIDs: []string{"ar-a"},
			check: func(t *testing.T, out []v1.ResultArtifact) {
				assert.LessOrEqual(t, len(out[0].Description), maxCopiedArtifactDescLen,
					"the cut description, marker included, must not exceed the bound")
				assert.Contains(t, out[0].Description, "truncated",
					"a cut description must say so rather than reading as a complete one")
			},
		},
		{
			// Truncating an ID would not shorten a handle, it would produce a
			// DIFFERENT one — naming some other render, or none, while reading
			// to the parent as the one the child returned.
			name:        "over-long id: dropped rather than truncated into a different handle",
			in:          []v1.ResultArtifact{{ID: longID, Description: "unusable"}, {ID: "ar-b", Description: "usable"}},
			wantIDs:     []string{"ar-b"},
			wantDropped: 1,
		},
		{
			name:        "empty id: dropped, since it names nothing the parent could attach",
			in:          []v1.ResultArtifact{{ID: "", Description: "names nothing"}},
			wantIDs:     nil,
			wantDropped: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, dropped := copyReturnedArtifacts(tc.in)

			gotIDs := make([]string, 0, len(out))
			for _, a := range out {
				gotIDs = append(gotIDs, a.ID)
			}
			assert.Equal(t, tc.wantIDs, nilIfEmpty(gotIDs))
			assert.Equal(t, tc.wantDropped, dropped)
			if tc.check != nil {
				require.NotEmpty(t, out, "the case's check needs an entry to inspect")
				tc.check(t, out)
			}
		})
	}
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}
