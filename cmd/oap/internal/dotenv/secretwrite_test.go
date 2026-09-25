package dotenv_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/dotenv"
)

func newSecretScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	return s
}

func TestWriteSecretKey(t *testing.T) {
	cases := []struct {
		name        string
		existing    *corev1.Secret
		wantCreated bool
		wantValue   string
	}{
		{
			name:        "new Secret: created=true, value written",
			existing:    nil,
			wantCreated: true,
			wantValue:   "abc123",
		},
		{
			name: "existing Secret: created=false, value overwritten",
			existing: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: "ns"},
				Type:       corev1.SecretTypeOpaque,
				Data:       map[string][]byte{"token": []byte("old-value")},
			},
			wantCreated: false,
			wantValue:   "new-value",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			b := fake.NewClientBuilder().WithScheme(newSecretScheme(t))
			if tc.existing != nil {
				b = b.WithObjects(tc.existing)
			}
			c := b.Build()

			created, err := dotenv.WriteSecretKey(ctx, c, "ns", "my-secret", "token", []byte(tc.wantValue))
			require.NoError(t, err)
			assert.Equal(t, tc.wantCreated, created)

			var sec corev1.Secret
			require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "my-secret"}, &sec))
			assert.Equal(t, tc.wantValue, string(sec.Data["token"]))
		})
	}
}
