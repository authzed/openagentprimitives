package settingswizard

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// fieldManager is the fixed SSA owner so re-runs converge on the same manager.
const fieldManager = "ap-settings-wizard"

// LoadExisting returns the current singleton "cluster" ClusterAgentSettings to
// pre-populate the wizard form. When the singleton does not yet exist, it
// returns a zero-value ClusterAgentSettings named "cluster" (nil error) so the
// wizard form starts empty rather than failing.
func LoadExisting(ctx context.Context, c client.Client) (*v1alpha1.ClusterAgentSettings, error) {
	var cas v1alpha1.ClusterAgentSettings
	err := c.Get(ctx, client.ObjectKey{Name: v1alpha1.ClusterAgentSettingsName}, &cas)
	if apierrors.IsNotFound(err) {
		return &v1alpha1.ClusterAgentSettings{
			ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.ClusterAgentSettingsName},
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get ClusterAgentSettings %q: %w", v1alpha1.ClusterAgentSettingsName, err)
	}
	return &cas, nil
}

// Apply server-side-applies the composed ClusterAgentSettings under the fixed
// field manager (force=true). SSA asserts ownership of the fields the wizard
// manages and overwrites them; it does NOT remove fields owned by a DIFFERENT
// manager (e.g. an install-seeded pinning rule, or a `oap settings apply -f`
// under the "ap-apply" manager) that this apply omits — those survive. For the
// wizard's curated security knobs this is the intended/fail-safe behavior.
// Converts the typed object to unstructured for the dynamic SSA path used
// across cmd/oap.
func Apply(ctx context.Context, dyn dynamic.Interface, cas *v1alpha1.ClusterAgentSettings) error {
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cas)
	if err != nil {
		return fmt.Errorf("convert ClusterAgentSettings to unstructured: %w", err)
	}
	obj := &unstructured.Unstructured{Object: u}
	if err := kube.Apply(ctx, dyn, obj, fieldManager); err != nil {
		return fmt.Errorf("apply ClusterAgentSettings %q: %w", cas.Name, err)
	}
	return nil
}
