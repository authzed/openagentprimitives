package meta

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// succeededWithArtifacts is the status a resolved delegation carries once the
// controller has copied what the child returned.
func succeededWithArtifacts(result string, artifacts ...v1.ResultArtifact) v1.SubagentRequestStatus {
	return v1.SubagentRequestStatus{
		Phase:     v1.SubagentRequestPhaseSucceeded,
		ChildRef:  &v1.NamespacedRef{Namespace: "ns", Name: "demo-child"},
		Result:    result,
		Artifacts: artifacts,
	}
}

// TestDelegateTool_SuccessCarriesTheReturnedArtifactHandles is the parent's
// half of the propagation. A result that mentioned a report while carrying no
// handle left the parent unable to deliver, unable to attach, and unable even
// to know an artifact existed.
func TestDelegateTool_SuccessCarriesTheReturnedArtifactHandles(t *testing.T) {
	const requestName = "subreq-demo-parent-abc12"
	tl := NewDelegateTool(DelegateConfig{
		Namespace: "ns", SessionName: "demo-parent",
		Create: func(_ context.Context, sr *v1.SubagentRequest) error {
			sr.Name = requestName // the apiserver names it; delegate polls by that name
			return nil
		},
		Poll: func(_ context.Context, _ string) (*v1.SubagentRequest, error) {
			return &v1.SubagentRequest{Status: succeededWithArtifacts("the report is ready",
				v1.ResultArtifact{ID: "ar-child-report", Description: "the findings report"},
			)}, nil
		},
		PollInterval: time.Millisecond,
	})

	res, err := tl.Execute(context.Background(),
		json.RawMessage(`{"agent":"demo-coder","task":"write the report"}`), nil)
	require.NoError(t, err)
	require.False(t, res.IsError, "content=%s", res.Content)

	assert.Contains(t, res.Content, "the report is ready", "the child's own answer still comes through")
	assert.Contains(t, res.Content, requestName+"/ar-child-report",
		"the handle must arrive in the delegated form respond_to_user accepts, not as a bare render name")
	assert.Contains(t, res.Content, "the findings report", "the child's description rides with its handle")
	assert.Contains(t, res.Content, "attached",
		"the model must be told the field that delivers it, or the handle is trivia")
	assert.False(t, res.Trusted,
		"handles ride the child's own words and must stay subject to the same content inspection")
}

// TestDelegateTool_SuccessWithNoArtifactsIsUnchanged pins that the ordinary
// delegation gains no boilerplate: a child that returned only words must not
// hand its parent a paragraph about artifacts it does not have.
func TestDelegateTool_SuccessWithNoArtifactsIsUnchanged(t *testing.T) {
	tl := NewDelegateTool(DelegateConfig{
		Namespace: "ns", SessionName: "demo-parent",
		Create: func(_ context.Context, sr *v1.SubagentRequest) error { sr.Name = "subreq-x"; return nil },
		Poll: func(_ context.Context, _ string) (*v1.SubagentRequest, error) {
			return &v1.SubagentRequest{Status: succeededWithArtifacts("just the answer")}, nil
		},
		PollInterval: time.Millisecond,
	})

	res, err := tl.Execute(context.Background(),
		json.RawMessage(`{"agent":"demo-coder","task":"answer"}`), nil)
	require.NoError(t, err)
	assert.Equal(t, "just the answer", res.Content)
}

// TestReplyToSubagent_SuccessCarriesTheReturnedArtifactHandles spans the join
// the two tools share. delegate and reply_to_subagent resolve through the same
// waitForNext, and a conversational delegation that ends after a clarification
// is exactly the one whose artifacts would be missed if only the first were
// covered.
func TestReplyToSubagent_SuccessCarriesTheReturnedArtifactHandles(t *testing.T) {
	const requestName = "subreq-demo-parent-x"
	sp := &scriptedPoll{states: []v1.SubagentRequestStatus{
		awaitingStatus(1, "which repo?"),
		succeededWithArtifacts("done", v1.ResultArtifact{ID: "ar-child-diff", Description: "the diff"}),
	}}
	tl := NewSubagentReplyTool(DelegateConfig{
		Namespace: "ns", SessionName: "demo-parent",
		Poll:         sp.poll,
		Send:         func(context.Context, string, string, string) error { return nil },
		PollInterval: time.Millisecond,
		Timeout:      time.Minute,
	})

	res, err := tl.Execute(context.Background(),
		json.RawMessage(`{"delegation":"`+requestName+`","message":"the second one"}`), nil)
	require.NoError(t, err)
	require.False(t, res.IsError, "content=%s", res.Content)
	assert.Contains(t, res.Content, requestName+"/ar-child-diff",
		"an artifact returned after a clarification must reach the parent in the same usable form")
}

func TestCutDelegatedArtifactHandle_Forms(t *testing.T) {
	cases := []struct {
		name           string
		in             string
		wantDelegation string
		wantArtifact   string
		wantOK         bool
	}{
		{
			name: "delegation and handle: split", in: "subreq-a/ar-b",
			wantDelegation: "subreq-a", wantArtifact: "ar-b", wantOK: true,
		},
		{
			// The tag may contain a slash, so this ordinary handle must fall
			// through to artifact resolution rather than being refused as an
			// unknown delegation.
			name: "tagged revision whose tag contains a slash: not delegated", in: "artifact-x#v1/final",
		},
		{name: "plain render name: not delegated", in: "ar-b"},
		{name: "plain artifact id: not delegated", in: "artifact-x"},
		{name: "empty delegation half: not delegated", in: "/ar-b"},
		{name: "empty artifact half: not delegated", in: "subreq-a/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			delegation, artifact, ok := cutDelegatedArtifactHandle(tc.in)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantDelegation, delegation)
			assert.Equal(t, tc.wantArtifact, artifact)
		})
	}
}
