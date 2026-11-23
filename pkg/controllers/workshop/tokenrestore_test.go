package workshop_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/controllers/workshop"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

func restoreWorkshop(session string, phase string, deleting bool) *spiceboxv1alpha1.Workshop {
	ws := sanctionedWorkshop(builderSession(session, session+"-uid"))
	ws.Status.Phase = phase
	if deleting {
		now := metav1.Now()
		ws.DeletionTimestamp = &now
		ws.Finalizers = []string{spiceboxv1alpha1.FinalizerWorkshop}
	}
	return ws
}

func workshopBearerSecret(session, token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: spiceboxv1alpha1.WorkshopTokenSecretName(session)},
		Data:       map[string][]byte{"token": []byte(token)},
	}
}

func workshopTokenKey(session string) memory.NamespacedName {
	return memory.NamespacedName{Namespace: "default", Name: spiceboxv1alpha1.WorkshopName(session)}
}

func TestRestoreTokens(t *testing.T) {
	scheme := testfixtures.NewScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		restoreWorkshop("ready", spiceboxv1alpha1.WorkshopPhaseReady, false),
		restoreWorkshop("provisioning", spiceboxv1alpha1.WorkshopPhaseProvisioning, false),
		restoreWorkshop("deleting", spiceboxv1alpha1.WorkshopPhaseReady, true),
		restoreWorkshop("no-secret", spiceboxv1alpha1.WorkshopPhaseReady, false),
		workshopBearerSecret("ready", "ready-tok"),
		workshopBearerSecret("provisioning", "provisioning-tok"),
		workshopBearerSecret("deleting", "deleting-tok"),
	).Build()

	reg := tokens.NewRegistry()
	reg.BeginRestore()
	r := &workshop.Reconciler{Client: c, Tokens: reg}

	n, err := r.RestoreTokens(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n, "only the Ready, live workshop with a Secret is restored")

	got, ok := reg.Lookup("ready-tok")
	assert.True(t, ok, "a Ready workshop's sidecar bearer is back")
	assert.Equal(t, workshopTokenKey("ready"), got)
	assert.False(t, reg.Registered(workshopTokenKey("provisioning")), "a workshop still provisioning is left to its own reconcile")
	assert.False(t, reg.Registered(workshopTokenKey("deleting")), "a deleting workshop is skipped: teardown may already have revoked it")
	assert.False(t, reg.Registered(workshopTokenKey("no-secret")), "no Secret, no token: restore never mints")
}

func TestRestoreTokens_SecretReadFailureIsReturned(t *testing.T) {
	scheme := testfixtures.NewScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		restoreWorkshop("broken", spiceboxv1alpha1.WorkshopPhaseReady, false),
		restoreWorkshop("fine", spiceboxv1alpha1.WorkshopPhaseReady, false),
		workshopBearerSecret("broken", "broken-tok"),
		workshopBearerSecret("fine", "fine-tok"),
	).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name == spiceboxv1alpha1.WorkshopTokenSecretName("broken") {
				return errors.New("apiserver hiccup")
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}).Build()

	reg := tokens.NewRegistry()
	r := &workshop.Reconciler{Client: c, Tokens: reg}

	n, err := r.RestoreTokens(context.Background())
	assert.ErrorContains(t, err, "apiserver hiccup", "a failed read is returned so the pass is retried")
	assert.Equal(t, 1, n, "and does not stop the other workshop's restore")
	assert.True(t, reg.Registered(workshopTokenKey("fine")))
	assert.False(t, reg.Registered(workshopTokenKey("broken")))
}

func TestRestoreTokens_ForbiddenReadIsSkippedNotRetried(t *testing.T) {
	scheme := testfixtures.NewScheme(t)
	forbiddenSecret := spiceboxv1alpha1.WorkshopTokenSecretName("forbidden")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		restoreWorkshop("forbidden", spiceboxv1alpha1.WorkshopPhaseReady, false),
		restoreWorkshop("fine", spiceboxv1alpha1.WorkshopPhaseReady, false),
		workshopBearerSecret("forbidden", "forbidden-tok"),
		workshopBearerSecret("fine", "fine-tok"),
	).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name == forbiddenSecret {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, key.Name, errors.New("no RBAC"))
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}).Build()

	reg := tokens.NewRegistry()
	r := &workshop.Reconciler{Client: c, Tokens: reg}

	n, err := r.RestoreTokens(context.Background())
	assert.NoError(t, err, "Forbidden will not clear on retry, so it must not hold the registry restoring")
	assert.Equal(t, 1, n)
	assert.True(t, reg.Registered(workshopTokenKey("fine")))
	assert.False(t, reg.Registered(workshopTokenKey("forbidden")), "left to the Workshop's own Reconcile")
}

func TestReconcile_WaitsForTokenRestore(t *testing.T) {
	sess := builderSession("builder", "uid-1")
	ws := sanctionedWorkshop(sess)
	reg := tokens.NewRegistry()
	reg.BeginRestore()
	_, r := newFakeReconciler(t, &fakeTuples{}, reg, sess, ws)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ws.Namespace, Name: ws.Name}})
	assert.ErrorIs(t, err, context.DeadlineExceeded, "nothing runs (teardown included) until the restore is done")
}
