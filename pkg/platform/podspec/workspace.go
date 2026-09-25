package podspec

import (
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// workspaceMountPath is where the shared workspace PVC is mounted in every
// bundle SpiceboxSession pod. Distinct from workMountPath (/work), which
// stays a pod-local emptyDir.
const workspaceMountPath = "/workspace"

// WorkspaceClaimName is the deterministic name of the PVC backing an
// AgentSession's shared workspace (ReadWriteOnce; see BuildWorkspacePVC).
func WorkspaceClaimName(sess *spiceboxv1alpha1.AgentSession) string {
	return sess.Name + "-workspace"
}
