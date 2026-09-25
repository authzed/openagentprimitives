package artifacts_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

func TestNewRender(t *testing.T) {
	spec := spiceboxv1alpha1.ArtifactRenderSpec{Kind: "html", Payload: []byte("<p>hi</p>"), TimeoutSeconds: 30}
	annos := map[string]string{artifacts.AnnoArtifactName: "Report"}

	t.Run("with a session UID: owned by the session, controller, blocking deletion", func(t *testing.T) {
		cr := artifacts.NewRender("ar-sess-live-abc123", "workshop", "sess-live", types.UID("uid-1"), "artifact-0123456789abcdef", spec, annos)
		assert.Equal(t, "ArtifactRender", cr.Kind)
		assert.Equal(t, spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(), cr.APIVersion)
		assert.Equal(t, "ar-sess-live-abc123", cr.Name)
		assert.Equal(t, "workshop", cr.Namespace)
		assert.Equal(t, "artifact-0123456789abcdef", cr.Labels[artifacts.LabelArtifactID])
		assert.Equal(t, "Report", cr.Annotations[artifacts.AnnoArtifactName])
		assert.Equal(t, spec, cr.Spec)
		require.Len(t, cr.OwnerReferences, 1)
		o := cr.OwnerReferences[0]
		assert.Equal(t, "AgentSession", o.Kind)
		assert.Equal(t, "sess-live", o.Name)
		assert.Equal(t, types.UID("uid-1"), o.UID)
		require.NotNil(t, o.Controller)
		assert.True(t, *o.Controller)
		require.NotNil(t, o.BlockOwnerDeletion)
		assert.True(t, *o.BlockOwnerDeletion)
	})

	t.Run("without a UID: no owner reference, everything else the same", func(t *testing.T) {
		cr := artifacts.NewRender("ar-x", "ns", "sess", "", "artifact-0123456789abcdef", spec, nil)
		assert.Empty(t, cr.OwnerReferences)
		assert.Nil(t, cr.Annotations)
		assert.Equal(t, "artifact-0123456789abcdef", cr.Labels[artifacts.LabelArtifactID])
	})
}
