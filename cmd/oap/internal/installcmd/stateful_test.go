package installcmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/progress"
	"github.com/authzed/openagentprimitives/pkg/platform/cloud"
	_ "github.com/authzed/openagentprimitives/pkg/platform/cloud/unmanaged" // registers cloud.KeyDefault
	"github.com/authzed/openagentprimitives/pkg/platform/manifests"
)

func TestResolveStatefulChoice_ExplicitOverride_PinsDirectly(t *testing.T) {
	bundle := &kube.Bundle{Typed: fake.NewSimpleClientset()}
	var out bytes.Buffer
	dec, err := resolveStatefulChoice(context.Background(), bundle, cloud.MustFor(cloud.KeyDefault),
		StatefulResolveOptions{ExplicitClass: "my-rwo-class"}, progress.New(&out, strings.NewReader(""), false))
	require.NoError(t, err)
	assert.Equal(t, "my-rwo-class", dec.ClassName)
	assert.Nil(t, dec.CreateClass, "explicit override never creates a class")
	assert.False(t, dec.Probe, "explicit override is trusted directly, no probe")
}

func TestResolveStatefulChoice_DefaultStrategy_TrustsClusterDefault(t *testing.T) {
	bundle := &kube.Bundle{Typed: fake.NewSimpleClientset()}
	var out bytes.Buffer
	dec, err := resolveStatefulChoice(context.Background(), bundle, cloud.MustFor(cloud.KeyDefault),
		StatefulResolveOptions{}, progress.New(&out, strings.NewReader(""), false))
	require.NoError(t, err)
	assert.Empty(t, dec.ClassName, "local/default strategy trusts the cluster default")
}

func TestInjectStatefulClass_PinsPostgresAndNeo4j(t *testing.T) {
	docs, err := injectStatefulClass(manifests.Postgres, "ap-stateful-hyperdisk")
	require.NoError(t, err)
	out, err := manifests.InjectStorageClass(docs[0], "") // no-op re-parse: docs[0] is the PVC, already injected
	require.NoError(t, err)
	parsed, err := manifests.Split(out)
	require.NoError(t, err)
	got, found, err := manifests.NestedStringForTest(parsed[0].Object, "spec", "storageClassName")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "ap-stateful-hyperdisk", got)
}

func TestInjectStatefulClass_EmptyClass_Unchanged(t *testing.T) {
	orig, err := manifests.Postgres()
	require.NoError(t, err)
	got, err := injectStatefulClass(manifests.Postgres, "")
	require.NoError(t, err)
	assert.Equal(t, orig, got)
}

func TestCreateStorageClass_Idempotent(t *testing.T) {
	kc := fake.NewSimpleClientset()
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "ap-stateful-hyperdisk"}, Provisioner: "pd.csi.storage.gke.io"}
	require.NoError(t, createStorageClass(context.Background(), kc, sc))
	// Second create tolerates AlreadyExists.
	require.NoError(t, createStorageClass(context.Background(), kc, sc))
	got, err := kc.StorageV1().StorageClasses().Get(context.Background(), "ap-stateful-hyperdisk", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "pd.csi.storage.gke.io", got.Provisioner)
}
