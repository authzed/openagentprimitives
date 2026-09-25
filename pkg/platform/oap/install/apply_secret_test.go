package install_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
)

// TestInstall_CreatedSecretIsAdoptedBySelf pins the one label a hand-created
// Secret lacks and an install-created one must not.
//
// The operator's manager cache label-filters its Secret informer on
// adoptguard.AdoptedLabel, and that label is only ever stamped BY a reconcile.
// A Secret carrying none fires no watch event, so the AgentIdentity that
// references it converges only on the reconciler's own 30s re-check — and a
// later rotation of that Secret's value is invisible to the watch entirely.
// Install knows the CRs it is applying reference this Secret, so it stamps the
// label at creation the same way every other operator-minted Secret does
// (adoptguard.WithAdoptedLabel, cmd/oap/internal/modeltoken.EnsureSecret).
func TestInstall_CreatedSecretIsAdoptedBySelf(t *testing.T) {
	const ns = "adopt-label-ns"

	c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()

	_, err := install.Install(context.Background(), c, bundleWithSoleAgentClass("label-agent"),
		oap.Answers{}, []install.SecretSpec{{Name: "widget-token", Key: "api-key", Value: "sk-fixture-value"}},
		install.InstallOpts{Namespace: ns})
	require.NoError(t, err, "Install must apply the bundle and create the Secret")

	got := &unstructured.Unstructured{}
	got.SetAPIVersion("v1")
	got.SetKind("Secret")
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "widget-token"}, got),
		"the SecretSpec must have been created")

	assert.Contains(t, got.GetLabels(), adoptguard.AdoptedLabel,
		"without the adoption label the operator's Secret watch never sees this Secret arrive")
	assert.Equal(t, "label-agent", got.GetLabels()[instance.LabelInstall],
		"the install labels are unchanged — they are what uninstall selects on")
}
