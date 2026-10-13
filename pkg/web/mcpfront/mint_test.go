package mcpfront

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/accesstoken"
)

// recordedCall is one call recorded by fakeGrantWriter, in the order it
// happened — the Minter test's only window onto "tuples before CR".
type recordedCall struct {
	method  string // "write" | "delete"
	tokenID string
	grant   spicedb.AccessTokenGrant
}

// fakeGrantWriter is the GrantWriter test double: it records every call it
// receives (and, via sharedOrder, interleaves with the fake K8s client's
// Create calls below) so ordering can be asserted directly.
type fakeGrantWriter struct {
	order     *[]string
	calls     []recordedCall
	writeErr  error
	deleteErr error
}

func (f *fakeGrantWriter) WriteAccessTokenGrant(_ context.Context, g spicedb.AccessTokenGrant) error {
	f.calls = append(f.calls, recordedCall{method: "write", tokenID: g.TokenID, grant: g})
	if f.order != nil {
		*f.order = append(*f.order, "write-grant")
	}
	return f.writeErr
}

func (f *fakeGrantWriter) DeleteAccessTokenTuples(_ context.Context, tokenID string) error {
	f.calls = append(f.calls, recordedCall{method: "delete", tokenID: tokenID})
	if f.order != nil {
		*f.order = append(*f.order, "delete-tuples")
	}
	return f.deleteErr
}

func newMintScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return s
}

func testOwner() identity.CanonicalUserID {
	return identity.CanonicalFromTrusted("alice", "test fixture")
}

func TestMintWritesTuplesThenCR(t *testing.T) {
	var order []string
	writer := &fakeGrantWriter{order: &order}
	c := fake.NewClientBuilder().
		WithScheme(newMintScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				order = append(order, "create-cr")
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()

	fixedNow := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := &Minter{
		SpiceDB:   writer,
		K8s:       c,
		Namespace: "agentprimitives-system",
		Lifetime:  90 * 24 * time.Hour,
		Clock:     func() time.Time { return fixedNow },
	}

	minted, err := m.MintAccessToken(context.Background(), MintParams{
		Owner:        testOwner(),
		Role:         accesstoken.RoleRead,
		ScopeClasses: []string{"default/demo-agent"},
		ClientName:   "demo tool",
		ClientID:     "client-abc",
	})
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(minted.Value, accesstoken.TokenPrefix))
	assert.NotEmpty(t, minted.TokenID)
	assert.Equal(t, fixedNow.Add(90*24*time.Hour), minted.ExpiresAt)

	require.Equal(t, []string{"write-grant", "create-cr"}, order, "tuples must be written before the CR is created")

	require.Len(t, writer.calls, 1)
	grant := writer.calls[0].grant
	assert.Equal(t, minted.TokenID, grant.TokenID)
	assert.Equal(t, testOwner(), grant.Owner)
	assert.Equal(t, accesstoken.RoleRead, grant.Role)
	assert.Equal(t, []string{"default/demo-agent"}, grant.ScopeClasses)
	assert.Equal(t, minted.ExpiresAt, grant.ExpiresAt, "the grant's expiry must be the SAME instant as the CR's")

	var tok spiceboxv1alpha1.AccessToken
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: "agentprimitives-system", Name: minted.TokenID}, &tok))
	assert.Equal(t, accesstoken.HashTokenValue(minted.Value), tok.Spec.TokenHash)
	assert.Equal(t, testOwner().String(), tok.Spec.Owner)
	assert.Equal(t, "demo tool", tok.Spec.ClientName)
	assert.Equal(t, "client-abc", tok.Spec.ClientID)
	assert.True(t, minted.ExpiresAt.Equal(tok.Spec.ExpiresAt.Time), "grant and CR expiry must be the same instant (minted=%v, cr=%v)", minted.ExpiresAt, tok.Spec.ExpiresAt.Time)
}

func TestMintCompensatesWhenCRCreateFails(t *testing.T) {
	writer := &fakeGrantWriter{}
	createErr := errors.New("apiserver unavailable")
	c := fake.NewClientBuilder().
		WithScheme(newMintScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				return createErr
			},
		}).Build()

	m := &Minter{
		SpiceDB:   writer,
		K8s:       c,
		Namespace: "agentprimitives-system",
		Lifetime:  90 * 24 * time.Hour,
	}

	_, err := m.MintAccessToken(context.Background(), MintParams{
		Owner:        testOwner(),
		Role:         accesstoken.RoleRead,
		ScopeClasses: []string{"default/demo-agent"},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, createErr, "the CR-create error must be part of the joined error")
	assert.Contains(t, err.Error(), "apiserver unavailable")

	require.Len(t, writer.calls, 2, "write then compensating delete")
	assert.Equal(t, "write", writer.calls[0].method)
	assert.Equal(t, "delete", writer.calls[1].method)
	assert.Equal(t, writer.calls[0].tokenID, writer.calls[1].tokenID, "the compensation must target the SAME token id the grant was written for")

	var list spiceboxv1alpha1.AccessTokenList
	require.NoError(t, c.List(context.Background(), &list))
	assert.Empty(t, list.Items, "a failed create must leave no CR behind")
}

func TestMintCompensationFailureIsJoined(t *testing.T) {
	createErr := errors.New("apiserver unavailable")
	deleteErr := errors.New("spicedb unreachable")
	writer := &fakeGrantWriter{deleteErr: deleteErr}
	c := fake.NewClientBuilder().
		WithScheme(newMintScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				return createErr
			},
		}).Build()

	m := &Minter{SpiceDB: writer, K8s: c, Namespace: "agentprimitives-system", Lifetime: time.Hour}

	_, err := m.MintAccessToken(context.Background(), MintParams{
		Owner: testOwner(), Role: accesstoken.RoleRead, Unfiltered: true,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, createErr)
	assert.ErrorIs(t, err, deleteErr, "a failed compensation must never be silently dropped")
}

func TestMintRejectsEmptyOwnerOrRole(t *testing.T) {
	cases := []struct {
		name   string
		params MintParams
	}{
		{name: "empty owner", params: MintParams{Role: accesstoken.RoleRead, Unfiltered: true}},
		{name: "unknown role", params: MintParams{Owner: testOwner(), Role: "bogus", Unfiltered: true}},
		{name: "no scope and not unfiltered", params: MintParams{Owner: testOwner(), Role: accesstoken.RoleRead}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writer := &fakeGrantWriter{}
			c := fake.NewClientBuilder().WithScheme(newMintScheme(t)).Build()
			m := &Minter{SpiceDB: writer, K8s: c, Namespace: "ns", Lifetime: time.Hour}

			_, err := m.MintAccessToken(context.Background(), tc.params)
			require.Error(t, err)
			assert.Empty(t, writer.calls, "a rejected mint must never reach SpiceDB")
		})
	}
}
