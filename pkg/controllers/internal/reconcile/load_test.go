package reconcile_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
)

func TestLoadInto(t *testing.T) {
	scheme := newScheme(t)
	existing := newPod("present")

	cases := []struct {
		name     string
		key      types.NamespacedName
		clientFn func(t *testing.T) client.Client
		wantCont bool
		wantErr  bool
	}{
		{
			name: "object exists: cont=true, no err",
			key:  types.NamespacedName{Namespace: "default", Name: "present"},
			clientFn: func(*testing.T) client.Client {
				return fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
			},
			wantCont: true,
		},
		{
			name:     "NotFound: cont=false, no err",
			key:      types.NamespacedName{Namespace: "default", Name: "missing"},
			clientFn: func(*testing.T) client.Client { return fake.NewClientBuilder().WithScheme(scheme).Build() },
			wantCont: false,
		},
		{
			name: "transient API error: cont=false, err propagated",
			key:  types.NamespacedName{Namespace: "default", Name: "present"},
			clientFn: func(t *testing.T) client.Client {
				return fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						return apierrors.NewServiceUnavailable("boom")
					},
				}).Build()
			},
			wantCont: false,
			wantErr:  true,
		},
		{
			name: "non-API error: cont=false, err propagated unchanged",
			key:  types.NamespacedName{Namespace: "default", Name: "present"},
			clientFn: func(t *testing.T) client.Client {
				return fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						return errors.New("wire failure")
					},
				}).Build()
			},
			wantCont: false,
			wantErr:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got corev1.Pod
			cont, err := apreconcile.LoadInto(context.Background(), tc.clientFn(t), tc.key, &got)
			assert.Equal(t, tc.wantCont, cont)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
