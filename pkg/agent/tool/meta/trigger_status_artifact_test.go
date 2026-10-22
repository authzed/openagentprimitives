package meta_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state/deliveries"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	ap "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

func publishArtifactFixture(t *testing.T, name string) (*triggerFake, *tool.SessionContext, meta.TriggerStatusConfig, *ap.ArtifactRender, string) {
	t.Helper()
	k := registerTriggerFake(t, name)
	sess := dlSessionWith(t, k.name)
	sess.AgentSessionUID = types.UID("publication-session")
	svc := newAwaitSvc()
	id := svc.NewArtifactID()
	cr := &ap.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{Name: "ar-publish", Namespace: sess.Namespace, UID: types.UID("render-publication"), Labels: map[string]string{artifacts.LabelArtifactID: id}, OwnerReferences: []metav1.OwnerReference{{APIVersion: ap.SchemeBuilder.GroupVersion.String(), Kind: "AgentSession", Name: sess.Name, UID: sess.AgentSessionUID}}},
		Spec:       ap.ArtifactRenderSpec{Kind: "html"},
		Status:     ap.ArtifactRenderStatus{Phase: ap.ArtifactRenderPhaseReady, OutputMIME: "text/html", OutputRef: "mem://report"},
	}
	require.NoError(t, sess.K8sClient.(client.Client).Create(memory.WithSystemApproval(context.Background(), "test"), cr))
	_, err := svc.FinalizeRevision(memory.WithSystemApproval(context.Background(), "test"), memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}, cr)
	require.NoError(t, err)
	cfg := dlConfig(k.name, dlWebdBase)
	cfg.Artifacts = svc
	cfg.ComposedTextOnly = true
	return k, sess, cfg, cr, id
}

func TestConcludeTriggerStatus_PublishesPreparedHTMLArtifact(t *testing.T) {
	k, sess, cfg, cr, id := publishArtifactFixture(t, "faketrigger-explicit-artifact")
	raw, _ := json.Marshal(map[string]string{"outcome": "clean", "artifact_id": id, "details_url": "https://foreign.example"})
	res, err := meta.NewConcludeTriggerStatus(cfg).Execute(memory.WithSystemApproval(context.Background(), "test"), raw, sess)
	require.NoError(t, err)
	require.False(t, res.IsError, res.Content)
	require.Len(t, k.concluded, 1)
	want, err := channelkinds.ComposeArtifactViewURL(dlWebdBase, sess.Namespace+"/"+sess.Name, id)
	require.NoError(t, err)
	assert.Equal(t, want, k.concluded[0].DetailsURL)
	assert.NotContains(t, k.concluded[0].Summary, "session’s channel")
	assert.Contains(t, k.concluded[0].Summary, "authenticated report")
	store, ok := deliveries.TryFrom(sess)
	require.True(t, ok)
	assert.True(t, store.Delivered(cr.Name))
	assert.Equal(t, id, store.LastArtifactID())
}

func TestConcludeTriggerStatus_FailedPublicationDoesNotDeliverArtifact(t *testing.T) {
	k, sess, cfg, cr, id := publishArtifactFixture(t, "faketrigger-artifact-provider-failure")
	k.concludeErr = errors.New("provider unavailable")
	raw, _ := json.Marshal(map[string]string{"outcome": "clean", "artifact_id": id})
	res, err := meta.NewConcludeTriggerStatus(cfg).Execute(memory.WithSystemApproval(context.Background(), "test"), raw, sess)
	require.NoError(t, err)
	require.True(t, res.IsError)
	store, _ := deliveries.TryFrom(sess)
	assert.False(t, store.Delivered(cr.Name))
	assert.Empty(t, store.LastArtifactID())
}

func TestConcludeTriggerStatus_RejectsInvalidPreparedArtifact(t *testing.T) {
	for _, tc := range []string{"foreign", "pending", "non-html", "no-webd"} {
		t.Run(tc, func(t *testing.T) {
			k, sess, cfg, cr, id := publishArtifactFixture(t, "faketrigger-artifact-invalid-"+tc)
			switch tc {
			case "foreign":
				cr.OwnerReferences[0].UID = "another-session"
			case "pending":
				cr.Status.Phase = ap.ArtifactRenderPhasePending
			case "non-html":
				cr.Spec.Kind = "image"
				cr.Status.OutputMIME = "image/png"
			case "no-webd":
				cfg.WebdBaseURL = func() string { return "" }
			}
			require.NoError(t, sess.K8sClient.(client.Client).Update(memory.WithSystemApproval(context.Background(), "test"), cr))
			raw, _ := json.Marshal(map[string]string{"outcome": "clean", "artifact_id": id})
			res, err := meta.NewConcludeTriggerStatus(cfg).Execute(memory.WithSystemApproval(context.Background(), "test"), raw, sess)
			require.NoError(t, err)
			assert.True(t, res.IsError, res.Content)
			assert.Empty(t, k.concluded)
			store, _ := deliveries.TryFrom(sess)
			assert.False(t, store.Delivered(cr.Name))
		})
	}
}
