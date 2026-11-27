//go:build integration

// Integration test for the AccessTokenSpec CEL rules: every spec field is
// immutable after create. Runs against a real envtest apiserver (via
// pkg/controllers/testenv) so the assertion exercises the generated CRD's
// XValidation rules directly — in particular that the has()-guarded rules for
// the optional fields, and `==` over the date-time expiresAt, survive the
// apiserver's CEL type-checker.
package v1alpha1_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

func TestAccessTokenSpecImmutableCEL(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()
	expires := metav1.NewTime(time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second))

	cases := []struct {
		name string
		// withClient creates the token with clientName/clientID set; false
		// leaves both optional fields absent.
		withClient bool
		mutate     func(at *v1alpha1.AccessToken)
		// wantErr is a substring of the expected rejection; empty means the
		// update must be accepted.
		wantErr string
	}{
		{name: "no change: accepted (a re-apply must not be rejected)", withClient: true, mutate: func(*v1alpha1.AccessToken) {}},
		{name: "metadata-only change: accepted (the controller adds its finalizer this way)", withClient: true,
			mutate: func(at *v1alpha1.AccessToken) { at.Finalizers = append(at.Finalizers, "test.example/hold") }},
		{name: "expiresAt extended: rejected", withClient: true,
			mutate:  func(at *v1alpha1.AccessToken) { at.Spec.ExpiresAt = metav1.NewTime(expires.Add(365 * 24 * time.Hour)) },
			wantErr: "expiresAt is immutable"},
		{name: "expiresAt shortened: rejected", withClient: true,
			mutate:  func(at *v1alpha1.AccessToken) { at.Spec.ExpiresAt = metav1.NewTime(expires.Add(-time.Hour)) },
			wantErr: "expiresAt is immutable"},
		{name: "clientName changed: rejected", withClient: true,
			mutate:  func(at *v1alpha1.AccessToken) { at.Spec.ClientName = "another-client" },
			wantErr: "clientName is immutable"},
		{name: "clientName removed: rejected", withClient: true,
			mutate:  func(at *v1alpha1.AccessToken) { at.Spec.ClientName = "" },
			wantErr: "clientName is immutable"},
		{name: "clientName added to a token minted without one: rejected", withClient: false,
			mutate:  func(at *v1alpha1.AccessToken) { at.Spec.ClientName = "late-client" },
			wantErr: "clientName is immutable"},
		{name: "clientID changed: rejected", withClient: true,
			mutate:  func(at *v1alpha1.AccessToken) { at.Spec.ClientID = "another-client-id" },
			wantErr: "clientID is immutable"},
		{name: "clientID removed: rejected", withClient: true,
			mutate:  func(at *v1alpha1.AccessToken) { at.Spec.ClientID = "" },
			wantErr: "clientID is immutable"},
		{name: "clientID added to a token minted without one: rejected", withClient: false,
			mutate:  func(at *v1alpha1.AccessToken) { at.Spec.ClientID = "late-client-id" },
			wantErr: "clientID is immutable"},
		{name: "owner changed: rejected", withClient: true,
			mutate:  func(at *v1alpha1.AccessToken) { at.Spec.Owner = "someone-else" },
			wantErr: "owner is immutable"},
		{name: "tokenHash changed: rejected", withClient: true,
			mutate:  func(at *v1alpha1.AccessToken) { at.Spec.TokenHash = strings.Repeat("b", 64) },
			wantErr: "tokenHash is immutable"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at := &v1alpha1.AccessToken{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("at-immutable-%02d", i), Namespace: "default"},
				Spec: v1alpha1.AccessTokenSpec{
					TokenHash: strings.Repeat("a", 64),
					Owner:     "demo-owner",
					ExpiresAt: expires,
				},
			}
			if tc.withClient {
				at.Spec.ClientName = "demo-client"
				at.Spec.ClientID = "demo-client-id"
			}
			require.NoError(t, env.Client.Create(ctx, at), "create")

			tc.mutate(at)
			err := env.Client.Update(ctx, at)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err, "the update must be rejected")
			assert.True(t, apierrors.IsInvalid(err), "want an Invalid rejection from the apiserver, got: %v", err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
