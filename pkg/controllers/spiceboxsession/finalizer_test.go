package spiceboxsession

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/exec"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
)

// fakeTeardownRuntime is a minimal sandboxkinds.Runtime whose Teardown call
// history and return value are test-controlled. Ensure/Status/Executor are
// not exercised by the finalizer path and panic if called, so a test that
// hits them fails loudly instead of silently passing on the wrong path.
type fakeTeardownRuntime struct {
	mu            sync.Mutex
	teardownCalls []sandboxkinds.Handle
	teardownErr   error
}

func (f *fakeTeardownRuntime) Ensure(context.Context, sandboxkinds.EnsureRequest) (sandboxkinds.Handle, error) {
	panic("fakeTeardownRuntime.Ensure: not exercised by the finalizer path")
}

func (f *fakeTeardownRuntime) Status(context.Context, sandboxkinds.Handle) (sandboxkinds.Status, error) {
	panic("fakeTeardownRuntime.Status: not exercised by the finalizer path")
}

func (f *fakeTeardownRuntime) Teardown(_ context.Context, h sandboxkinds.Handle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.teardownCalls = append(f.teardownCalls, h)
	return f.teardownErr
}

func (f *fakeTeardownRuntime) Executor(sandboxkinds.Handle) (exec.Executor, error) {
	panic("fakeTeardownRuntime.Executor: not exercised by the finalizer path")
}

func (f *fakeTeardownRuntime) Watches() []sandboxkinds.Watch { return nil }

func (f *fakeTeardownRuntime) calls() []sandboxkinds.Handle {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sandboxkinds.Handle(nil), f.teardownCalls...)
}

var _ sandboxkinds.Runtime = (*fakeTeardownRuntime)(nil)

// deletingSession builds a SpiceboxSession already mid-deletion: finalizer
// present, DeletionTimestamp set. The fake client's WithObjects seeds the
// tracker directly (no real apiserver create-validation), so this is a valid
// starting fixture even though a real POST could never carry a
// DeletionTimestamp — see pkg/controllers/sidecartoolbox/controller_test.go
// for the same pattern.
func deletingSession(name string, sandbox *spiceboxv1alpha1.SandboxHandle) *spiceboxv1alpha1.SpiceboxSession {
	now := metav1.Now()
	return &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			Finalizers:        []string{spiceboxv1alpha1.FinalizerSpiceboxSession},
			DeletionTimestamp: &now,
		},
		Status: spiceboxv1alpha1.SpiceboxSessionStatus{Sandbox: sandbox},
	}
}

// finalizerScheme mirrors ttl_test.go's ttlScheme for this package.
func finalizerScheme(t *testing.T) *apiruntime.Scheme {
	t.Helper()
	s := apiruntime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return s
}

// assertFinalizerGone checks the canonical "finalized" signal: either the
// object is gone entirely, or it remains but no longer carries the
// finalizer. The fake client's behavior once finalizers empty on a
// DeletionTimestamp'd object varies by version, so a NotFound Get is treated
// as a pass rather than asserted against.
func assertFinalizerGone(t *testing.T, c client.Client, name string) {
	t.Helper()
	var got spiceboxv1alpha1.SpiceboxSession
	err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, &got)
	if err != nil {
		return // object fully removed by the fake client; that's finalized too
	}
	for _, f := range got.Finalizers {
		assert.NotEqual(t, spiceboxv1alpha1.FinalizerSpiceboxSession, f,
			"finalizer must be removed once deletion cleanup completes")
	}
}

func TestReconcile_Deletion(t *testing.T) {
	cases := []struct {
		name                 string
		sandbox              *spiceboxv1alpha1.SandboxHandle
		runtimes             func() (sandboxkinds.Runtimes, *fakeTeardownRuntime)
		wantErr              bool
		wantFinalizerRemoved bool
		wantTeardownCalls    int
	}{
		{
			// Group (c)'s Critical fix: a bound session's Teardown is routed
			// through the seam, not a raw pod Get/Delete.
			name:    "bound session: Teardown is called through the seam, then finalizes",
			sandbox: &spiceboxv1alpha1.SandboxHandle{Kind: "pod", Ref: "default/sess-a-pod"},
			runtimes: func() (sandboxkinds.Runtimes, *fakeTeardownRuntime) {
				rt := &fakeTeardownRuntime{}
				return sandboxkinds.Runtimes{"pod": rt}, rt
			},
			wantFinalizerRemoved: true,
			wantTeardownCalls:    1,
		},
		{
			// A session that never bound to a backend has nothing to tear
			// down; Runtimes is deliberately empty to prove For is never
			// even consulted.
			name:    "status.Sandbox nil: finalizes cleanly, no runtime lookup",
			sandbox: nil,
			runtimes: func() (sandboxkinds.Runtimes, *fakeTeardownRuntime) {
				return sandboxkinds.Runtimes{}, nil
			},
			wantFinalizerRemoved: true,
			wantTeardownCalls:    0,
		},
		{
			// A transient Teardown failure must not orphan the sandbox: the
			// finalizer stays and the reconcile is retried.
			name:    "Teardown errors: finalizer stays, error surfaces for retry",
			sandbox: &spiceboxv1alpha1.SandboxHandle{Kind: "pod", Ref: "default/sess-c-pod"},
			runtimes: func() (sandboxkinds.Runtimes, *fakeTeardownRuntime) {
				rt := &fakeTeardownRuntime{teardownErr: fmt.Errorf("delete pod: connection refused")}
				return sandboxkinds.Runtimes{"pod": rt}, rt
			},
			wantErr:              true,
			wantFinalizerRemoved: false,
			wantTeardownCalls:    1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := deletingSession("sess-under-test", tc.sandbox)
			c := fake.NewClientBuilder().WithScheme(finalizerScheme(t)).WithObjects(sess).Build()
			runtimes, rt := tc.runtimes()
			r := &Reconciler{Client: c, Runtimes: runtimes}

			_, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: sess.Name, Namespace: sess.Namespace},
			})
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			if rt != nil {
				assert.Len(t, rt.calls(), tc.wantTeardownCalls, "Teardown call count")
				if tc.wantTeardownCalls > 0 {
					assert.Equal(t, sandboxkinds.Handle{Kind: tc.sandbox.Kind, Ref: tc.sandbox.Ref}, rt.calls()[0],
						"Teardown must receive the stored handle verbatim")
				}
			}

			if tc.wantFinalizerRemoved {
				assertFinalizerGone(t, c, sess.Name)
			} else {
				var got spiceboxv1alpha1.SpiceboxSession
				require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: sess.Name, Namespace: "default"}, &got))
				assert.Contains(t, got.Finalizers, spiceboxv1alpha1.FinalizerSpiceboxSession,
					"finalizer must remain while teardown is retried")
			}
		})
	}
}

// TestReconcile_Deletion_MissingRuntimeLogsAndFinalizes covers the
// deliberate asymmetry with the create path: kind resolution on CREATE is
// fail-closed (an unresolvable kind marks the session invalid and blocks
// progress), but on DELETE it must not be — refusing to finalize would make
// the session permanently undeletable. A missing runtime here proceeds to
// remove the finalizer, but must never do so silently: an operator needs the
// log line to know a real resource may have been orphaned.
func TestReconcile_Deletion_MissingRuntimeLogsAndFinalizes(t *testing.T) {
	sandbox := &spiceboxv1alpha1.SandboxHandle{Kind: "ghost-kind", Ref: "some-external-id"}
	sess := deletingSession("sess-orphan-risk", sandbox)
	c := fake.NewClientBuilder().WithScheme(finalizerScheme(t)).WithObjects(sess).Build()
	// Runtimes deliberately has no "ghost-kind" entry — simulates a backend
	// that was unregistered or unlinked since the session bound.
	r := &Reconciler{Client: c, Runtimes: sandboxkinds.Runtimes{}}

	var msgs []string
	logger := funcr.New(func(prefix, args string) {
		msgs = append(msgs, prefix+" "+args)
	}, funcr.Options{})
	ctx := ctrllog.IntoContext(context.Background(), logger)

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: sess.Name, Namespace: sess.Namespace},
	})
	require.NoError(t, err, "a missing runtime must not fail-closed on delete")

	assertFinalizerGone(t, c, sess.Name)

	found := false
	for _, m := range msgs {
		if strings.Contains(m, sess.Name) && strings.Contains(m, sess.Namespace) && strings.Contains(m, "ghost-kind") {
			found = true
			break
		}
	}
	assert.True(t, found, "expected a log line naming the session, namespace and orphaned kind; got %v", msgs)
}
