package meta_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

func seedRev(t *testing.T, svc *artifacts.Service, scope memory.Scope, name, head, parent, change string, uid types.UID) artifacts.RevisionResult {
	t.Helper()
	cr := &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: uid, Labels: map[string]string{artifacts.LabelArtifactID: head}, Annotations: map[string]string{artifacts.AnnoChangeDescription: change}},
		Spec:       spiceboxv1alpha1.ArtifactRenderSpec{Kind: "html"},
		Status:     spiceboxv1alpha1.ArtifactRenderStatus{Phase: spiceboxv1alpha1.ArtifactRenderPhaseReady, OutputRef: "mem://" + name, OutputMIME: "text/html"},
	}
	if parent != "" {
		cr.Annotations[artifacts.AnnoParentRevision] = parent
	}
	r, err := svc.FinalizeRevision(memory.WithSystemApproval(context.Background(), "test"), scope, cr)
	require.NoError(t, err)
	return r
}

func TestArtifactHistory_ListsTree(t *testing.T) {
	svc := artifacts.NewService(memory.NewLocal(inmem.NewBackend()), nil)
	scope := memory.Scope{Kind: "session", ID: "default/sess1"}
	head := svc.NewArtifactID()
	r1 := seedRev(t, svc, scope, "ar-1", head, "", "v1", "u1")
	seedRev(t, svc, scope, "ar-2", head, r1.RevisionID, "v2", "u2")

	tl := meta.NewArtifactHistory(meta.ArtifactHistoryConfig{Artifacts: svc})
	args, _ := json.Marshal(map[string]any{"artifact": head})
	res, _ := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1"})
	require.False(t, res.IsError, "got: %s", res.Content)
	assert.Contains(t, res.Content, `"seq":2`)
	assert.Contains(t, res.Content, `"change_description":"v2"`)
	assert.True(t, res.Trusted, "artifact_history is a framework meta tool and must opt out of content-guard inspection")
}

func TestArtifactHistory_ListsArtifacts(t *testing.T) {
	svc := artifacts.NewService(memory.NewLocal(inmem.NewBackend()), nil)
	scope := memory.Scope{Kind: "session", ID: "default/sess1"}
	head := svc.NewArtifactID()
	seedRev(t, svc, scope, "ar-1", head, "", "v1", "u1")

	tl := meta.NewArtifactHistory(meta.ArtifactHistoryConfig{Artifacts: svc})
	args, _ := json.Marshal(map[string]any{}) // no artifact → list all
	res, _ := tl.Execute(memory.WithSystemApproval(context.Background(), "test"), args, &tool.SessionContext{Namespace: "default", Name: "sess1"})
	require.False(t, res.IsError, "got: %s", res.Content)
	assert.Contains(t, res.Content, head, "listing must include the artifact id")
}
