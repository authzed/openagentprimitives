package meta_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

const (
	parentNS   = "default"
	parentName = "demo-parent"
	parentUID  = types.UID("u-parent")
	childName  = "reqd-child"
	delegation = "subreq-demo-parent-abc12"
)

// childRender is an ArtifactRender the delegated child owns — the shape the
// parent must be able to deliver and could not before the handles crossed the
// boundary.
func childRender(name string, owner string, phase spiceboxv1alpha1.ArtifactRenderPhase) *spiceboxv1alpha1.ArtifactRender {
	return &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: parentNS,
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "AgentSession", Name: owner, UID: types.UID("u-" + owner),
			}},
		},
		Status: spiceboxv1alpha1.ArtifactRenderStatus{
			Phase: phase, OutputMIME: "text/html", OutputFilename: name + ".html",
		},
	}
}

// succeededDelegation is a SubagentRequest resolved by the controller: parented
// by parentName, having run childName, carrying the handles the child returned.
func succeededDelegation(parent string, returned ...string) *spiceboxv1alpha1.SubagentRequest {
	artifacts := make([]spiceboxv1alpha1.ResultArtifact, 0, len(returned))
	for _, id := range returned {
		artifacts = append(artifacts, spiceboxv1alpha1.ResultArtifact{ID: id, Description: "the report"})
	}
	return &spiceboxv1alpha1.SubagentRequest{
		ObjectMeta: metav1.ObjectMeta{Name: delegation, Namespace: parentNS},
		Spec: spiceboxv1alpha1.SubagentRequestSpec{
			Parent: spiceboxv1alpha1.NamespacedRef{Namespace: parentNS, Name: parent},
			Class:  "demo-coder", Task: "write the report",
		},
		Status: spiceboxv1alpha1.SubagentRequestStatus{
			Phase:     spiceboxv1alpha1.SubagentRequestPhaseSucceeded,
			ChildRef:  &spiceboxv1alpha1.NamespacedRef{Namespace: parentNS, Name: childName},
			Result:    "the report is ready",
			Artifacts: artifacts,
		},
	}
}

// respondWith builds respond_to_user over a fake cluster holding objs, and
// returns the tool plus a counter of published envelopes.
func respondWith(t *testing.T, objs ...client.Object) (tool.Tool, *int, *[]byte) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	publishes := 0
	var published []byte
	tl := meta.New(meta.RespondConfig{
		Capabilities: []string{"text", "asset:text/html"},
		ChannelKind:  "slack",
		Client:       c,
		NATSPublish: func(_ context.Context, _ string, payload []byte) error {
			publishes++
			published = append([]byte(nil), payload...)
			return nil
		},
		NATSSubjectPrefix: "ap.session." + parentNS + "." + parentName,
	})
	return tl, &publishes, &published
}

func respondAttaching(t *testing.T, tl tool.Tool, handle string) tool.Result {
	t.Helper()
	args, err := json.Marshal(map[string]any{"text": "here it is", "attached": []string{handle}})
	require.NoError(t, err, "marshal args")
	res, err := tl.Execute(context.Background(), args, &tool.SessionContext{
		Namespace: parentNS, Name: parentName, AgentSessionUID: parentUID,
	})
	require.NoError(t, err, "respond_to_user must refuse through IsError, never a fatal error")
	return res
}

// TestRespond_DelegatedHandle_ParentDeliversWhatItsChildReturned is the end of
// the propagation: the parent names a handle its child returned and the bytes
// reach the channel. Everything before this — the status field, the copy, the
// tool result — exists to put this call within reach.
func TestRespond_DelegatedHandle_ParentDeliversWhatItsChildReturned(t *testing.T) {
	tl, publishes, published := respondWith(t,
		succeededDelegation(parentName, "ar-child-report"),
		childRender("ar-child-report", childName, spiceboxv1alpha1.ArtifactRenderPhaseReady))

	res := respondAttaching(t, tl, delegation+"/ar-child-report")

	assert.False(t, res.IsError, "content=%s", res.Content)
	assert.Equal(t, 1, *publishes, "the reply must go out carrying the child's artifact")
	assert.Contains(t, string(*published), "ar-child-report",
		"the envelope must name the render the delegation returned")
}

// TestRespond_DelegatedHandle_RefusesWhatWasNotHandedOver is the guard that
// keeps the delegated form from being a second, looser door onto every render
// in the namespace. Each case is a different way the entitlement fails, and
// none of them may publish.
func TestRespond_DelegatedHandle_RefusesWhatWasNotHandedOver(t *testing.T) {
	cases := []struct {
		name    string
		objs    []client.Object
		handle  string
		wantMsg string
	}{
		{
			name: "delegation belongs to another parent: refused, not yours to deliver",
			objs: []client.Object{
				succeededDelegation("some-other-parent", "ar-child-report"),
				childRender("ar-child-report", childName, spiceboxv1alpha1.ArtifactRenderPhaseReady),
			},
			handle:  delegation + "/ar-child-report",
			wantMsg: "is not yours",
		},
		{
			name: "handle the delegation never returned: refused even though the render exists",
			objs: []client.Object{
				succeededDelegation(parentName, "ar-child-report"),
				childRender("ar-child-private", childName, spiceboxv1alpha1.ArtifactRenderPhaseReady),
			},
			handle:  delegation + "/ar-child-private",
			wantMsg: "returned no artifact",
		},
		{
			name: "render owned by neither the child nor this session: refused",
			objs: []client.Object{
				succeededDelegation(parentName, "ar-stranger"),
				childRender("ar-stranger", "unrelated-session", spiceboxv1alpha1.ArtifactRenderPhaseReady),
			},
			handle:  delegation + "/ar-stranger",
			wantMsg: "does not own",
		},
		{
			name: "delegation that does not exist: refused with the delegation named",
			objs: []client.Object{
				childRender("ar-child-report", childName, spiceboxv1alpha1.ArtifactRenderPhaseReady),
			},
			handle:  delegation + "/ar-child-report",
			wantMsg: "does not exist here",
		},
		{
			// The render being Ready is still required for a delegated handle:
			// the delegation vouches for WHO may deliver it, never for whether
			// there are bytes to deliver.
			name: "render the child returned is not ready: refused",
			objs: []client.Object{
				succeededDelegation(parentName, "ar-child-report"),
				childRender("ar-child-report", childName, spiceboxv1alpha1.ArtifactRenderPhasePending),
			},
			handle:  delegation + "/ar-child-report",
			wantMsg: "not ready",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tl, publishes, _ := respondWith(t, tc.objs...)

			res := respondAttaching(t, tl, tc.handle)

			assert.True(t, res.IsError, "content=%s", res.Content)
			assert.Equal(t, 0, *publishes, "a refused attachment must deliver no message at all")
			assert.Contains(t, res.Content, tc.wantMsg)
			assert.Contains(t, res.Content, tc.handle, "the refusal must name the handle the model passed")
		})
	}
}

// TestRespond_TaggedHandleWithASlashIsNotReadAsDelegated pins the separator's
// one real collision. A revision tag may contain "/", so
// "artifact-x#v1/final" is an ORDINARY handle; reading it as a delegated one
// would refuse an ordinary attachment with an error about delegations the
// agent never made.
func TestRespond_TaggedHandleWithASlashIsNotReadAsDelegated(t *testing.T) {
	tl, publishes, _ := respondWith(t)

	res := respondAttaching(t, tl, "artifact-x#v1/final")

	assert.True(t, res.IsError, "the handle names nothing in this fixture, so it must still fail")
	assert.Equal(t, 0, *publishes)
	assert.NotContains(t, res.Content, "delegation",
		"a tagged handle must fail as a missing artifact, never as a missing delegation")
}
