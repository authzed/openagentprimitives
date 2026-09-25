package projectors

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
)

// newClient builds a fake client over the project scheme seeded with objs.
// Status subresource is intentionally NOT split out (no WithStatusSubresource)
// so the seeded status conditions survive into List — the projectors read
// status directly.
func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	return fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
}

// cond builds a single status condition.
func cond(condType string, status metav1.ConditionStatus, reason string) metav1.Condition {
	return metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		LastTransitionTime: metav1.Now(),
	}
}

// rowByName finds the row with metadata-name Name (and optional kind badge),
// failing the test if absent. kind == "" matches any row with that name.
func rowByName(t *testing.T, rows []config.ResourceRow, name, kind string) config.ResourceRow {
	t.Helper()
	for _, r := range rows {
		if r.Name != name {
			continue
		}
		if kind == "" || badgeVal(r, "kind") == kind {
			return r
		}
	}
	require.Failf(t, "row not found", "no row name=%q kind=%q in %+v", name, kind, rows)
	return config.ResourceRow{}
}

// badgeVal returns the value of the first badge with key, or "".
func badgeVal(r config.ResourceRow, key string) string {
	for _, b := range r.Badges {
		if b.Key == key {
			return b.Value
		}
	}
	return ""
}

// countVal returns the value of the first count with label, or -1 when absent.
func countVal(r config.ResourceRow, label string) int {
	for _, c := range r.Counts {
		if c.Label == label {
			return c.Value
		}
	}
	return -1
}
