package wizardrun

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Apply's writes are server-side applies with ForceOwnership, and the refusal
// in front of them covered the CHANNEL only. So a caller naming an existing
// Secret had its DATA replaced with the wizard's answers — and admind takes
// that channel name over HTTP, where the install path's conflict machinery does
// not run at all.
//
// The test is the InstalledByAnnotation rather than mere existence, because
// re-setup with a fresh token legitimately rewrites OUR Secret. Refusing that
// would break the flow this route exists for.

func secretClient(t *testing.T, objs ...runtime.Object) *fake.ClientBuilder {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(objs...)
}

func TestRefuseForeignSecret_RefusesOneThisProjectDidNotCreate(t *testing.T) {
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "app-creds", Namespace: "apps"},
		Data:       map[string][]byte{"token": []byte("someone-elses")},
	}
	c := secretClient(t, foreign).Build()

	err := refuseForeignSecret(context.Background(), c, "apps", "app-creds")

	require.Error(t, err, "a Secret with no installed-by annotation belongs to something else")
	assert.Contains(t, err.Error(), "app-creds", "the refusal names the object so the operator can resolve it")
}

func TestRefuseForeignSecret_AllowsRewritingOurOwn(t *testing.T) {
	ours := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "app-creds", Namespace: "apps",
			Annotations: map[string]string{InstalledByAnnotation: "admind"},
		},
	}
	c := secretClient(t, ours).Build()

	assert.NoError(t, refuseForeignSecret(context.Background(), c, "apps", "app-creds"),
		"re-setup with a fresh token rewrites the Secret this project created")
}

func TestRefuseForeignSecret_AbsentSecretIsACreate(t *testing.T) {
	c := secretClient(t).Build()

	assert.NoError(t, refuseForeignSecret(context.Background(), c, "apps", "app-creds"))
}

// An offline run has no cluster to read and none to clobber, matching
// RefuseExisting's own nil-client behaviour.
func TestRefuseForeignSecret_NilClientRefusesNothing(t *testing.T) {
	assert.NoError(t, refuseForeignSecret(context.Background(), nil, "apps", "app-creds"))
}
