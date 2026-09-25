package workspace

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LabelWorkspaceJob marks a POD created by one of this package's workspace
// Jobs. Its value is the job's component ("workspace-reconcile", …).
//
// It exists because a NetworkPolicy selects PODS, and these pods carried no
// labels at all — the component label lived on the Job's own ObjectMeta, which
// selects nothing. NetworkPolicy is an allow-union with no implicit default, so
// a pod no policy selects is unrestricted in BOTH directions even on a fully
// enforcing CNI. The per-session policies all select session labels, and these
// pods carry none, so every workspace Job ran with unrestricted egress AND
// accepted connections from anywhere in the cluster.
//
// That is not hypothetical for the reconcile Job in particular: it runs git
// against a remote with an injected upstream credential, from a pod that also
// mounts the shared workspace volume the agent has been writing to.
//
// This label does not itself constrain anything — it makes constraint possible,
// for a policy this project stamps or one a cluster operator writes. The
// component value is preserved rather than flattened to a boolean so a policy
// can distinguish "the git one" from the local copy Jobs, which need no egress
// at all.
const LabelWorkspaceJob = "agentprimitives.authzed.com/workspace-job"

// jobPodMeta returns the pod-template metadata for a workspace Job of the given
// component, merging any extra labels the caller needs on the pod.
//
// A single constructor rather than a literal per Job: five templates that each
// spell their own labels are five places for the next one to be forgotten, and a
// forgotten one is silent — the Job runs fine, it is simply outside every
// policy.
func jobPodMeta(component string, extra map[string]string) metav1.ObjectMeta {
	labels := map[string]string{
		"app.kubernetes.io/component": component,
		LabelWorkspaceJob:             component,
	}
	for k, v := range extra {
		labels[k] = v
	}
	return metav1.ObjectMeta{Labels: labels}
}
