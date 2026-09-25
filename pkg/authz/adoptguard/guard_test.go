package adoptguard

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func sec(ns, name string, labels map[string]string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels}}
}

// newSecretGuard wires a Guard[*corev1.Secret] over a live reader holding objs.
// (Reads go through the live Reader; the Cache field is reserved/unused.)
func newSecretGuard(t *testing.T, mode Mode, objs ...client.Object) *Guard[*corev1.Secret] {
	t.Helper()
	reader := fake.NewClientBuilder().WithObjects(objs...).Build()
	return &Guard[*corev1.Secret]{
		Reader:      reader,
		Cache:       reader,
		Allowlisted: func(nn types.NamespacedName) bool { return nn.Namespace == "sys" && nn.Name == "infra" },
		Mode:        mode,
		New:         func() *corev1.Secret { return &corev1.Secret{} },
	}
}

func TestGuard_AllowsAdopted(t *testing.T) {
	g := newSecretGuard(t, Panic, sec("ns", "ok", map[string]string{AdoptedLabel: "true"}))
	got, err := g.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "ok"})
	require.NoError(t, err)
	assert.Equal(t, "ok", got.Name)
}

func TestGuard_AllowsAllowlisted(t *testing.T) {
	// Allowlisted object is returned even though it carries no adoption label.
	g := newSecretGuard(t, Panic, sec("sys", "infra", nil))
	got, err := g.Get(context.Background(), types.NamespacedName{Namespace: "sys", Name: "infra"})
	require.NoError(t, err)
	assert.Equal(t, "infra", got.Name)
}

func TestGuard_PanicsOnExistingNonAdopted(t *testing.T) {
	// The object EXISTS but is neither adopted nor allowlisted — overreach ⇒ panic.
	g := newSecretGuard(t, Panic, sec("ns", "evil", nil))
	assert.Panics(t, func() {
		_, _ = g.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "evil"})
	})
}

func TestGuard_AbsentReturnsNotFound(t *testing.T) {
	// Absence is NOT overreach: a non-existent object returns NotFound, never panics.
	g := newSecretGuard(t, Panic)
	_, err := g.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "ghost"})
	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(err), "absent object ⇒ NotFound, got %v", err)
}

func TestGuard_WarnModeReturnsExistingNonAdopted(t *testing.T) {
	// Warn mode: an existing non-adopted object is logged loudly + returned (bring-up).
	g := newSecretGuard(t, Warn, sec("ns", "evil", nil))
	got, err := g.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "evil"})
	require.NoError(t, err)
	assert.Equal(t, "evil", got.Name)
}

func TestWithAdoptedLabel_SetsLabel(t *testing.T) {
	s := &corev1.Secret{}
	WithAdoptedLabel(s)
	assert.Contains(t, s.GetLabels(), AdoptedLabel)
}
