package sessionevents

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	events "github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	meminmem "github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type access struct {
	denied  string
	checked []events.Dependency
}

func (a *access) CheckOnResource(_ context.Context, typ, id, permission string, _ identity.CanonicalUserID, consistent bool) (bool, error) {
	a.checked = append(a.checked, events.Dependency{ResourceType: typ, ResourceID: id, Permission: permission})
	return consistent && id != a.denied, nil
}
func TestNativeSessionAccessPinsIncarnationAndAllFlowDependencies(t *testing.T) {
	ctx := memory.WithCaller(memory.WithSystemApproval(context.Background(), "system:operator"), "system:operator")
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	sess := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "source", UID: "original"}}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sess).Build()
	mem := memory.NewLocal(meminmem.NewBackend())
	auth := &access{}
	native := &NativeSessions{Reader: k8s, Memory: mem, Auth: auth}
	source, err := native.Resolve(ctx, "owner", events.Source{Kind: "native", Namespace: "team", ID: "team/source"})
	require.NoError(t, err)
	require.Equal(t, "original", source.UID)
	dep := events.Dependency{ResourceType: "document", ResourceID: "trip-note", Permission: "read"}
	require.NoError(t, native.Check(ctx, "owner", source, []events.Dependency{dep}))
	require.Contains(t, auth.checked, dep)
	auth.denied = "trip-note"
	require.ErrorIs(t, native.Check(ctx, "owner", source, []events.Dependency{dep}), events.ErrDenied)
	auth.denied = ""
	// Current source taints also guard old evidence, even if the event envelope
	// did not supply the new dependency. Production memory verifies these writes.
	raw, err := json.Marshal(infoleakagetaint.TaintRecord{ResourceType: "document", ResourceID: "trip-note", Permission: "read"})
	require.NoError(t, err)
	_, err = mem.Put(ctx, memory.Entry{Scope: memory.Scope{Kind: "session", ID: source.ID}, Kind: infoleakagetaint.KindName, ID: "ilt-test", CreatedAt: time.Now().UTC(), Content: raw})
	require.NoError(t, err)
	auth.denied = "trip-note"
	require.ErrorIs(t, native.Check(ctx, "owner", source, nil), events.ErrDenied)
	auth.denied = ""
	require.NoError(t, k8s.Delete(ctx, sess))
	require.NoError(t, k8s.Create(ctx, &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "source", UID: "replacement"}}))
	require.ErrorIs(t, native.Check(ctx, "owner", source, nil), events.ErrDenied)
	_, err = native.Resolve(ctx, "owner", source)
	require.ErrorIs(t, err, events.ErrDenied, "an explicit stale UID cannot silently pin the replacement")
	deps, err := native.CheckSource(ctx, source, memory.Entry{Scope: memory.Scope{Kind: "session", ID: source.ID}})
	require.ErrorIs(t, err, events.ErrDenied)
	require.Empty(t, deps)
	var replacement v1.AgentSession
	require.NoError(t, k8s.Get(ctx, client.ObjectKeyFromObject(sess), &replacement))
	require.EqualValues(t, "replacement", replacement.UID)
}

type incompleteNativeMemory struct {
	memory.Memory
	result memory.QueryResult
	err    error
}

func (m incompleteNativeMemory) Query(context.Context, memory.Query) (memory.QueryResult, error) {
	return m.result, m.err
}
func TestNativeAccessRefusesIncompleteOrMalformedDependencies(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	sess := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "source", UID: "original"}}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sess).Build()
	for _, tc := range []struct {
		name   string
		result memory.QueryResult
		err    error
	}{
		{name: "partial", result: memory.QueryResult{Partial: true}},
		{name: "truncated", result: memory.QueryResult{Truncated: true}},
		{name: "malformed JSON", result: memory.QueryResult{Entries: []memory.Entry{{Content: json.RawMessage(`{"resourceType":`)}}}},
		{name: "missing permission", result: memory.QueryResult{Entries: []memory.Entry{{Content: json.RawMessage(`{"resourceType":"document","resourceID":"restricted"}`)}}}},
		{name: "store outage", err: errors.New("taint store unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := &access{}
			n := &NativeSessions{Reader: k8s, Memory: incompleteNativeMemory{result: tc.result, err: tc.err}, Auth: auth}
			err := n.Check(ctx, "owner", events.Source{Kind: "native", Namespace: "team", ID: "team/source", UID: "original"}, nil)
			require.Error(t, err, "missing dependency evidence cannot permit unattended work")
		})
	}
}
