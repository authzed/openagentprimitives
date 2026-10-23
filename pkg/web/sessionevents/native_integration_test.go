//go:build integration

package sessionevents

import (
	"context"
	"testing"

	events "github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/memory"
	meminmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/test/testspicedb"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNativeSourceAuthorityWithRealSpiceDB(t *testing.T) {
	endpoint := testspicedb.SharedEndpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchema(t, endpoint, token)
	cl, err := spicedb.NewClient(endpoint, token, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cl.Close()) })
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	sess := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "source", UID: "original"}}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sess).Build()
	native := &NativeSessions{Reader: k8s, Memory: memory.NewLocal(meminmem.NewBackend()), Auth: cl}
	ctx := context.Background()
	owner := identity.CanonicalFromTrusted("YWxpY2VAZXhhbXBsZS5jb20", "test principal")
	source := events.Source{Kind: "native", Namespace: "team", ID: "team/source", UID: "original"}
	require.ErrorIs(t, native.Check(ctx, owner.String(), source, nil), events.ErrDenied, "a real source without read authority must be denied")
	require.NoError(t, cl.TouchStartedBy(ctx, "team", "source", owner))
	require.NoError(t, native.Check(ctx, owner.String(), source, nil), "native source mapping must ask an existing schema permission")
	resolved, err := native.Resolve(ctx, owner.String(), events.Source{Kind: "native", Namespace: "team", ID: "team/source"})
	require.NoError(t, err)
	require.Equal(t, source, resolved)
	require.NoError(t, cl.TouchDeniedUser(ctx, "team", "source", owner))
	require.ErrorIs(t, native.Check(ctx, owner.String(), source, nil), events.ErrDenied, "revocation must win over the starter grant")
}
