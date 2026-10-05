package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"testing"
	"time"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagedecision"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestClosureDenialReadUsesOperatorApprovalWithoutElevatingSignalCaller(t *testing.T) {
	for _, decision := range []string{infoleakagedecision.DecisionApproved, infoleakagedecision.DecisionDenied} {
		t.Run(decision, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, v1.AddToScheme(scheme))
			root := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "root"}}
			child := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "child", Labels: map[string]string{v1.LabelDelegationRoot: "root"}}, Spec: v1.AgentSessionSpec{Parent: &v1.NamespacedRef{Namespace: "ns", Name: "root"}}}
			k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, child).WithStatusSubresource(root, child).Build()
			mem := memory.NewLocal(inmem.NewBackend())
			scope := memory.Scope{Kind: "session", ID: "ns/child"}
			content, err := json.Marshal(infoleakagedecision.DecisionRecord{ResourceType: "document", ResourceID: "private", Decision: decision, At: time.Now().UTC()})
			require.NoError(t, err)
			signed := provenance.NewSigningMemory(mem, provenance.NewSigner(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)), "system:operator"))
			_, err = signed.Put(memory.WithSystemApproval(context.Background(), "test"), memory.Entry{Scope: scope, Kind: infoleakagedecision.KindName, ID: "ild-test", CreatedAt: time.Now().UTC(), Content: content})
			require.NoError(t, err)
			// Exactly the approval-less context carried by the live signal route.
			ctx := memory.WithTokenSession(context.Background(), memory.NamespacedName{Namespace: "ns", Name: "child"})
			_, err = infoleakagedecision.List(ctx, mem, scope)
			require.ErrorIs(t, err, memory.ErrMissingApproval)
			require.NoError(t, newClosureDenialStamper(k8s, mem).OnSignal(ctx, memory.Signal{Scope: scope}))
			for _, name := range []string{"root", "child"} {
				var got v1.AgentSession
				require.NoError(t, k8s.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: name}, &got))
				if decision == infoleakagedecision.DecisionDenied {
					require.NotNil(t, got.Status.ClosureDenied)
					require.True(t, *got.Status.ClosureDenied)
				} else {
					require.Nil(t, got.Status.ClosureDenied)
				}
			}
			_, err = infoleakagedecision.List(ctx, mem, scope)
			require.ErrorIs(t, err, memory.ErrMissingApproval, "approval must remain local to the operator read")
		})
	}
}
