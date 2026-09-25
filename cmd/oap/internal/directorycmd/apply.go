package directorycmd

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
)

// fieldManager is the fixed SSA owner for `oap directory configure`, so a
// re-run converges on the same manager rather than fighting a prior run for
// field ownership.
const fieldManager = "oap-directory-configure"

// Apply server-side-applies the RelationshipSource sel describes, under
// fieldManager, into namespace ns. The applied object is built directly
// as unstructured.Unstructured — mirroring the shape existing.go reads
// (spec.kind, spec.auth.agentIdentity/credential, spec.baseURL,
// spec.config) rather than round-tripping through the typed
// v1alpha1.RelationshipSource, so an optional field left blank in sel can
// be left out of the map entirely instead of relying on a json "omitempty"
// tag to do it on this command's behalf.
//
// The applied spec is a pure function of sel: no timestamp, no generated
// id, nothing read from the cluster beyond what sel already carries. A
// byte-identical sel therefore produces a byte-identical apply payload, so
// a re-run is a genuine SSA no-op rather than field-ownership churn that
// re-reconciles every watcher of the object.
//
// Only spec and metadata are set — status is the controller's, written
// through the status subresource, and a client that applied a status
// stanza here would fight it for ownership on every reconcile.
func Apply(ctx context.Context, dyn dynamic.Interface, ns string, sel Selections) error {
	spec := map[string]interface{}{
		"kind": sel.Kind,
		"auth": map[string]interface{}{
			"agentIdentity": sel.Identity,
			"credential":    sel.Credential,
		},
	}
	// An empty BaseURL is "no endpoint chosen", not "endpoint set to the
	// empty string" — see RelationshipSourceSpec.BaseURL's doc. Setting the
	// key at all would claim spec.baseURL under fieldManager and write ""
	// where the CRD (and credhost.Check, which tests spec.baseURL != "")
	// expects the field absent.
	if sel.Endpoint != "" {
		spec["baseURL"] = sel.Endpoint
	}
	if len(sel.Config) > 0 {
		var cfg interface{}
		if err := json.Unmarshal(sel.Config, &cfg); err != nil {
			return fmt.Errorf("unmarshal config for RelationshipSource %q: %w", sel.Name, err)
		}
		spec["config"] = cfg
	}

	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "agentprimitives.authzed.com/v1alpha1",
		"kind":       "RelationshipSource",
		"metadata": map[string]interface{}{
			"name":      sel.Name,
			"namespace": ns,
		},
		"spec": spec,
	}}

	if err := kube.Apply(ctx, dyn, obj, fieldManager); err != nil {
		return fmt.Errorf("apply RelationshipSource %q: %w", sel.Name, err)
	}
	return nil
}
