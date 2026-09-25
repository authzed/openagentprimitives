package tool

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// OwnedBySession returns true iff at least one ownerRef matches the calling
// AgentSession by Kind+Name+UID.
//
// UID is the cross-rebirth guard: a deleted+recreated session with the same
// name has a different UID and so cannot resurrect another session's objects.
//
// One home, because it is one question asked on several object kinds by several
// callers — respond_to_user validating an attachment, artifact_prepare
// resolving a parent revision, request_credential_update reattaching to its own
// pending request, and the artifact-delivered completion requirement listing
// what this session rendered. Four spellings of "is this mine?" is how one of
// them ends up accepting another session's object.
func OwnedBySession(refs []metav1.OwnerReference, sess *SessionContext) bool {
	if sess == nil {
		return false
	}
	for _, r := range refs {
		if r.Kind == "AgentSession" && r.Name == sess.Name && r.UID == sess.AgentSessionUID {
			return true
		}
	}
	return false
}
