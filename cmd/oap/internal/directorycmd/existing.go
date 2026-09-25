// Package directorycmd is the `oap directory` command family: configuring a
// RelationshipSource (a directory-sync kind — Slack, GitHub, ...) from the
// CLI.
package directorycmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// relationshipSourceGVR is the dynamic-client GVR for RelationshipSource,
// matching kube.Apply's CRD-derived pluralization (see
// pkg/apis/v1alpha1/relationshipsource_types.go).
var relationshipSourceGVR = schema.GroupVersionResource{
	Group: "agentprimitives.authzed.com", Version: "v1alpha1", Resource: "relationshipsources",
}

// AmbiguousSourceError is ExistingFor's refusal when more than one
// RelationshipSource names the same kind. It is a distinct type — not a
// plain fmt.Errorf — so a caller like `oap directory list` can tell this
// refusal apart from a genuine API failure via errors.As: the ambiguity is
// a legitimate shape (see ExistingFor's own doc) that a read-only listing
// can report per-kind and keep going past, where an API failure must still
// abort the whole listing.
type AmbiguousSourceError struct {
	// Kind is the relsync.Kind name every match shares.
	Kind string
	// Namespace is where the search ran.
	Namespace string
	// Names are the competing RelationshipSource CR names, in list order.
	Names []string
}

func (e *AmbiguousSourceError) Error() string {
	return fmt.Sprintf(
		"more than one RelationshipSource configures kind %q in namespace %q: %s — "+
			"pick one (delete or rename the others) before re-running; "+
			"this command cannot edit one member of the set without silently dropping the rest",
		e.Kind, e.Namespace, strings.Join(e.Names, ", "))
}

// ExistingFor reads what is already configured for one relsync kind in ns,
// so a re-run of the CLI wizard can prefill from it instead of asking from
// scratch. The zero relsync.ExistingConfig means nothing is configured yet —
// the ordinary first run, not an error.
//
// The CRD permits more than one RelationshipSource naming the same kind —
// github.com alongside a self-hosted GHES instance, for instance — and that
// is a legitimate shape, not a misconfiguration. But this command has no way
// yet to ask "which one did you mean", and settingswizard.Selections' own
// PreservedModelCatalog field exists because of exactly what happens when a
// wizard edits one member of a set it does not model: a forced server-side
// apply of the edited member carries forward nothing about the others, so
// they are silently dropped. Guessing which CR the caller meant (newest?
// first alphabetically?) would risk that same loss under a different name.
// So when more than one CR names the same kind, ExistingFor refuses outright
// and names every match — the honest answer until a future iteration of this
// command grows a way to pick one.
func ExistingFor(ctx context.Context, dyn dynamic.Interface, ns, kind string) (relsync.ExistingConfig, error) {
	list, err := dyn.Resource(relationshipSourceGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return relsync.ExistingConfig{}, fmt.Errorf("listing RelationshipSource in %q: %w", ns, err)
	}

	var matches []unstructured.Unstructured
	for _, item := range list.Items {
		itemKind, _, err := unstructured.NestedString(item.Object, "spec", "kind")
		if err != nil {
			return relsync.ExistingConfig{}, fmt.Errorf("reading spec.kind of RelationshipSource %q: %w", item.GetName(), err)
		}
		if itemKind == kind {
			matches = append(matches, item)
		}
	}

	switch len(matches) {
	case 0:
		return relsync.ExistingConfig{}, nil
	case 1:
		return existingConfigFrom(matches[0])
	default:
		names := make([]string, 0, len(matches))
		for _, m := range matches {
			names = append(names, m.GetName())
		}
		return relsync.ExistingConfig{}, &AmbiguousSourceError{Kind: kind, Namespace: ns, Names: names}
	}
}

// existingConfigFrom maps one matched RelationshipSource onto
// relsync.ExistingConfig. spec.config carries through uninterpreted — this
// command does not parse it, the kind does — but it is not guaranteed
// byte-identical to what is stored: obj.Object is dynamic.Interface's
// already-decoded map[string]interface{}, so reading spec.config back out
// through json.Marshal can reorder object keys. relsync.ExistingConfig's doc
// explains why that is fine — nothing downstream depends on byte identity.
func existingConfigFrom(obj unstructured.Unstructured) (relsync.ExistingConfig, error) {
	cfg := relsync.ExistingConfig{Name: obj.GetName()}

	var err error
	if cfg.Endpoint, _, err = unstructured.NestedString(obj.Object, "spec", "baseURL"); err != nil {
		return relsync.ExistingConfig{}, fmt.Errorf("reading spec.baseURL of RelationshipSource %q: %w", obj.GetName(), err)
	}
	if cfg.Identity, _, err = unstructured.NestedString(obj.Object, "spec", "auth", "agentIdentity"); err != nil {
		return relsync.ExistingConfig{}, fmt.Errorf("reading spec.auth.agentIdentity of RelationshipSource %q: %w", obj.GetName(), err)
	}
	if cfg.Credential, _, err = unstructured.NestedString(obj.Object, "spec", "auth", "credential"); err != nil {
		return relsync.ExistingConfig{}, fmt.Errorf("reading spec.auth.credential of RelationshipSource %q: %w", obj.GetName(), err)
	}

	configVal, found, err := unstructured.NestedFieldNoCopy(obj.Object, "spec", "config")
	if err != nil {
		return relsync.ExistingConfig{}, fmt.Errorf("reading spec.config of RelationshipSource %q: %w", obj.GetName(), err)
	}
	if found {
		raw, err := json.Marshal(configVal)
		if err != nil {
			return relsync.ExistingConfig{}, fmt.Errorf("marshalling spec.config of RelationshipSource %q: %w", obj.GetName(), err)
		}
		cfg.Config = raw
	}

	return cfg, nil
}
