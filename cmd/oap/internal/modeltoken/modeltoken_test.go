package modeltoken_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/modeltoken"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
)

func TestResolve(t *testing.T) {
	t.Run("file wins over env", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "tok")
		require.NoError(t, os.WriteFile(p, []byte("from-file\n"), 0o600))
		t.Setenv("MODEL_TOK", "from-env")
		got, err := modeltoken.Resolve(p, "MODEL_TOK", nil)
		require.NoError(t, err)
		assert.Equal(t, "from-file", got, "file value, trimmed")
	})

	t.Run("env used when no file", func(t *testing.T) {
		t.Setenv("MODEL_TOK", "from-env")
		got, err := modeltoken.Resolve("", "MODEL_TOK", nil)
		require.NoError(t, err)
		assert.Equal(t, "from-env", got)
	})

	t.Run("prompt used when no file and empty env", func(t *testing.T) {
		got, err := modeltoken.Resolve("", "MODEL_TOK_UNSET", func() (string, error) {
			return "from-prompt", nil
		})
		require.NoError(t, err)
		assert.Equal(t, "from-prompt", got)
	})

	t.Run("fail closed: no file, empty env, no prompt", func(t *testing.T) {
		_, err := modeltoken.Resolve("", "MODEL_TOK_UNSET", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "MODEL_TOK_UNSET")
	})
}

func TestEnsureSecret_StampsAdoptedLabel(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	secretGVR := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)

	ref := modeltoken.SecretRef{Namespace: "agentprimitives-system", Name: "model-default-token", Key: "token"}
	require.NoError(t, modeltoken.EnsureSecret(context.Background(), dyn, ref, "sk-test-value"))

	got, err := dyn.Resource(secretGVR).Namespace(ref.Namespace).Get(context.Background(), ref.Name, metav1.GetOptions{})
	require.NoError(t, err)
	// Adoption label present, or the operator's guarded reader panics on read.
	labels := got.GetLabels()
	assert.Equal(t, "true", labels[adoptguard.AdoptedLabel])
	// Value landed under the requested key (stringData is server-decoded to data
	// by the real apiserver; the fake stores stringData verbatim).
	sd, _, _ := unstructured.NestedStringMap(got.Object, "stringData")
	assert.Equal(t, "sk-test-value", sd["token"])
}
