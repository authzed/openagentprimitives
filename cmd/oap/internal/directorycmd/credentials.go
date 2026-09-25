package directorycmd

import (
	"context"
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// agentIdentityGVR is the dynamic-client GVR for AgentIdentity, matching
// kube.Apply's CRD-derived pluralization (see
// pkg/apis/v1alpha1/agentidentity_types.go).
var agentIdentityGVR = schema.GroupVersionResource{
	Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "agentidentities",
}

// CredentialRef names one selectable credential: an AgentIdentity paired
// with one of its spec.credentials[].name entries.
type CredentialRef struct {
	Identity   string
	Credential string
}

// AvailableCredentials lists every credential a directory-sync source in ns
// could be configured to use: every spec.credentials[].name on every
// AgentIdentity in the namespace. A sync's credential is not tied to a
// channel, so every credential on every identity is a candidate — this does
// not filter on AllowedHosts. That field scopes where a credential may be
// SENT and is enforced by the RelationshipSource controller (credhost.Check
// against spec.baseURL) once a source is written, not here: a credential
// with no AllowedHosts is legitimate for a kind that needs no endpoint at
// all, so treating an empty AllowedHosts as disqualifying here would be
// wrong.
//
// An empty result is not an error — it is a state the wizard must be able
// to explain to a human. Writing a RelationshipSource that names a
// credential which does not exist produces a source that reports
// Ready=False forever, so the wizard needs to distinguish "no credentials
// configured yet" from "the API call failed"; collapsing both into an error
// would make that impossible.
//
// The result is sorted by (Identity, Credential) so it is deterministic
// across calls — an unstable order would move a re-run's prefilled default
// under the operator between runs.
func AvailableCredentials(ctx context.Context, dyn dynamic.Interface, ns string) ([]CredentialRef, error) {
	list, err := dyn.Resource(agentIdentityGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing AgentIdentity in %q: %w", ns, err)
	}

	var refs []CredentialRef
	for _, item := range list.Items {
		creds, _, err := unstructured.NestedSlice(item.Object, "spec", "credentials")
		if err != nil {
			return nil, fmt.Errorf("reading spec.credentials of AgentIdentity %q: %w", item.GetName(), err)
		}
		for _, c := range creds {
			credMap, ok := c.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("spec.credentials entry of AgentIdentity %q is not an object: %T", item.GetName(), c)
			}
			name, _, err := unstructured.NestedString(credMap, "name")
			if err != nil {
				return nil, fmt.Errorf("reading name of a spec.credentials entry of AgentIdentity %q: %w", item.GetName(), err)
			}
			refs = append(refs, CredentialRef{Identity: item.GetName(), Credential: name})
		}
	}

	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Identity != refs[j].Identity {
			return refs[i].Identity < refs[j].Identity
		}
		return refs[i].Credential < refs[j].Credential
	})

	return refs, nil
}
