package settingswizard

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// defaultModelTokenRef returns the central token Secret coordinates every
// default catalog entry this package writes points at. A fresh pointer per
// call, never a shared one, so two entries can never alias the same struct.
// These MUST match settingscmd's defaultModelSecret* constants and the desktop
// configureHook's model-default-token Secret — they are the one central token
// the operator resolves the inherited default's key from.
func defaultModelTokenRef() *v1alpha1.NamespacedSecretKeyRef {
	return &v1alpha1.NamespacedSecretKeyRef{
		Namespace: "agentprimitives-system",
		Name:      "model-default-token",
		Key:       "token",
	}
}

// UpdateDefaultModelEntry sets the provider+name of the ClusterAgentSettings
// singleton's DEFAULT model catalog entry IN PLACE, touching nothing else — not
// its TokenRef, not any other catalog entry, and not the rest of the spec.
//
// It is the targeted counterpart to the full --defaults wizard path (see
// settingscmd.RunWizard): the settings UI's Model tab saves ONE model, and
// re-running the whole wizard would force-SSA the entire ClusterAgentSettings
// under the wizard's field manager — the model catalog is an atomic list, so
// that replaces every entry with the single default and resets the other
// security knobs to baseline. This does a spec-replacing Update instead, so
// every field the operator set elsewhere (the extra catalog entries, the
// pinning ceiling, the content inspectors) survives untouched.
//
// When a Default:true entry already exists, its Provider/Name are overwritten
// and everything else about it — its TokenRef especially — is preserved. When
// none does, a new default entry pointing at the standard central token Secret
// is appended. When the singleton does not exist at all, it is created carrying
// only that one entry. The write uses the fixed wizard field manager so a
// later full wizard run converges on the same owner rather than fighting it.
func UpdateDefaultModelEntry(ctx context.Context, c client.Client, provider, name string) error {
	var cas v1alpha1.ClusterAgentSettings
	getErr := c.Get(ctx, client.ObjectKey{Name: v1alpha1.ClusterAgentSettingsName}, &cas)
	switch {
	case apierrors.IsNotFound(getErr):
		cas = v1alpha1.ClusterAgentSettings{
			TypeMeta: metav1.TypeMeta{
				APIVersion: v1alpha1.SchemeGroupVersion.String(),
				Kind:       "ClusterAgentSettings",
			},
			ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ClusterAgentSettingsName},
		}
		entries := []v1alpha1.ModelCatalogEntry{{
			Name: name, Provider: provider, Default: true, TokenRef: defaultModelTokenRef(),
		}}
		cas.Spec.ModelCatalog = &entries
		if err := c.Create(ctx, &cas, client.FieldOwner(fieldManager)); err != nil {
			return fmt.Errorf("create ClusterAgentSettings %q: %w", v1alpha1.ClusterAgentSettingsName, err)
		}
		return nil
	case getErr != nil:
		return fmt.Errorf("get ClusterAgentSettings %q: %w", v1alpha1.ClusterAgentSettingsName, getErr)
	}

	entries := []v1alpha1.ModelCatalogEntry{}
	if cas.Spec.ModelCatalog != nil {
		entries = *cas.Spec.ModelCatalog
	}
	found := false
	for i := range entries {
		if entries[i].Default {
			entries[i].Provider = provider
			entries[i].Name = name
			found = true
			break
		}
	}
	if !found {
		entries = append(entries, v1alpha1.ModelCatalogEntry{
			Name: name, Provider: provider, Default: true, TokenRef: defaultModelTokenRef(),
		})
	}
	cas.Spec.ModelCatalog = &entries
	if err := c.Update(ctx, &cas, client.FieldOwner(fieldManager)); err != nil {
		return fmt.Errorf("update ClusterAgentSettings %q: %w", v1alpha1.ClusterAgentSettingsName, err)
	}
	return nil
}
