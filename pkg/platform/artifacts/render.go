package artifacts

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// NewRender builds the ArtifactRender CR every creator of one uses — the
// runner's artifact_prepare and its widget path, and the operator's workshop
// draft route — so the ownership shape is written once: owned by the session
// (by UID, as controller, blocking deletion) whenever the UID is known, the
// artifact id in LabelArtifactID, the head's intent in annotations. The
// ownership is what respond_to_user and artifact_await check before they will
// touch a render, so three hand-built copies of it were three places for the
// check to stop meaning the same thing.
//
// name is minted by the caller: NewRenderName for a model-facing handle, the
// widget path's own format for a widget. A nil annotations map is left nil.
func NewRender(name, namespace, session string, sessionUID types.UID, artifactID string, spec spiceboxv1alpha1.ArtifactRenderSpec, annotations map[string]string) *spiceboxv1alpha1.ArtifactRender {
	cr := &spiceboxv1alpha1.ArtifactRender{
		TypeMeta: metav1.TypeMeta{APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(), Kind: "ArtifactRender"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      map[string]string{LabelArtifactID: artifactID},
			Annotations: annotations,
		},
		Spec: spec,
	}
	if sessionUID != "" {
		tval := true
		cr.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(), Kind: "AgentSession",
			Name: session, UID: sessionUID, Controller: &tval, BlockOwnerDeletion: &tval,
		}}
	}
	return cr
}
