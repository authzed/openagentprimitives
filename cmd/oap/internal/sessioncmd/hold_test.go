package sessioncmd

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// fakeHoldChecker is a scriptable holdPermissionChecker. The real check
// (does agentsession#hold actually resolve through owner/parent/denied) is
// proven against a live SpiceDB schema in pkg/authz/spicedb/hold_integration_test.go;
// this fake exists so runSessionHold's WIRING — that it calls the check, and
// reacts correctly to true/false/error — is provable without a cluster.
type fakeHoldChecker struct {
	allowed bool
	err     error
}

func (f fakeHoldChecker) CheckHold(context.Context, string, string, identity.CanonicalUserID, bool) (bool, error) {
	return f.allowed, f.err
}

// installHoldChecker overrides newHoldChecker for the duration of the test
// and restores the real (SpiceDB-from-env) factory on cleanup.
func installHoldChecker(t *testing.T, checker holdPermissionChecker, connectErr error) {
	t.Helper()
	prev := newHoldChecker
	newHoldChecker = func() (holdPermissionChecker, error) { return checker, connectErr }
	t.Cleanup(func() { newHoldChecker = prev })
}

// alwaysAllowHold installs a fake that always grants agentsession#hold, for
// tests whose subject is the CR-creation path, not the authorization gate.
func alwaysAllowHold(t *testing.T) {
	t.Helper()
	installHoldChecker(t, fakeHoldChecker{allowed: true}, nil)
}

func TestHoldCmd_requiresReason(t *testing.T) {
	cmd := newHoldCmd()
	cmd.SetArgs([]string{"demo-session"})
	err := cmd.Execute()
	require.Error(t, err, "a hold with no reason produces a card nobody can act on")
	assert.Contains(t, err.Error(), "reason")
}

func TestBuildHold_setsManualSource(t *testing.T) {
	h := buildHold("demo", "demo-session", "looks wrong", "user:alice")
	assert.Equal(t, "manual", h.Spec.Source)
	assert.Equal(t, "demo-session", h.Spec.SessionRef.Name)
	assert.Equal(t, "looks wrong", h.Spec.Reason)
}

func TestBuildHold_carriesNoTimestamp(t *testing.T) {
	// The trip time is stamped by the controller into status. A CLI-set spec
	// timestamp would make re-running the command non-idempotent under SSA.
	a := buildHold("demo", "demo-session", "r", "user:alice")
	b := buildHold("demo", "demo-session", "r", "user:alice")
	assert.Equal(t, a.Spec, b.Spec, "two identical invocations must produce identical spec")
}

// TestRunSessionHold_sessionNotFound_returnsClearError proves the existence
// check runs, and its message surfaces, even with a checker that WOULD grant
// agentsession#hold — the Get happens before the permission check (both are
// gated by the caller's own Kubernetes RBAC, so the ordering opens no
// information channel; existence-first just keeps the two error messages
// precise instead of one masking the other). alwaysAllowHold is deliberate
// here, not incidental: it proves the not-found error surfaces regardless of
// what the check would have decided, because the check never runs for a
// session that doesn't exist.
func TestRunSessionHold_sessionNotFound_returnsClearError(t *testing.T) {
	alwaysAllowHold(t)
	b := aptest.NewBundle(t) // no AgentSession seeded
	g := aptest.GlobalsFor(b)

	err := runSessionHold(context.Background(), io.Discard, g, "demo-session", "looks wrong", "user:alice")
	require.Error(t, err, "a typo'd session name must not silently create an ownerless hold")
	assert.Contains(t, err.Error(), "demo-session", "the error must name the session the human typed")

	var holds spiceboxv1alpha1.SessionHoldList
	require.NoError(t, b.Controller.List(context.Background(), &holds, client.InNamespace("default")))
	assert.Empty(t, holds.Items, "a not-found session must not create a SessionHold CR")
}

func TestRunSessionHold_createsHoldOwnedByTheSession(t *testing.T) {
	alwaysAllowHold(t)
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo-session",
			Namespace: "default",
			UID:       "fixture-uid-1234",
		},
	}
	b := aptest.NewBundle(t, sess)
	g := aptest.GlobalsFor(b)

	require.NoError(t, runSessionHold(context.Background(), io.Discard, g, "demo-session", "looks wrong", "user:alice"))

	var holds spiceboxv1alpha1.SessionHoldList
	require.NoError(t, b.Controller.List(context.Background(), &holds, client.InNamespace("default")))
	require.Len(t, holds.Items, 1, "exactly one SessionHold must be created")

	h := holds.Items[0]
	assert.Equal(t, "manual", h.Spec.Source)
	assert.Equal(t, "demo-session", h.Spec.SessionRef.Name)
	assert.Equal(t, "looks wrong", h.Spec.Reason)
	assert.Equal(t, identity.Subject("user:alice"), h.Spec.TrippedBy)

	// activeHoldFor (pkg/controllers/agentsession/hold.go) discovers holds by
	// listing the namespace and matching spec.sessionRef.Name, not by
	// owner-ref — so an ownerless hold would outlive a deleted session and
	// freeze a same-named successor. The owner-ref is what makes the hold's
	// lifetime match the session's.
	require.Len(t, h.OwnerReferences, 1, "the hold must carry an owner-ref to its AgentSession")
	owner := h.OwnerReferences[0]
	assert.Equal(t, "AgentSession", owner.Kind)
	assert.Equal(t, "demo-session", owner.Name)
	assert.Equal(t, sess.UID, owner.UID)
	require.NotNil(t, owner.Controller)
	assert.True(t, *owner.Controller)
}

func TestRunSessionHold_calledTwice_createsTwoDistinctHolds(t *testing.T) {
	alwaysAllowHold(t)
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo-session",
			Namespace: "default",
			UID:       "fixture-uid-1234",
		},
	}
	b := aptest.NewBundle(t, sess)
	g := aptest.GlobalsFor(b)

	require.NoError(t, runSessionHold(context.Background(), io.Discard, g, "demo-session", "first reason", "user:alice"))
	require.NoError(t, runSessionHold(context.Background(), io.Discard, g, "demo-session", "second reason", "user:alice"))

	var holds spiceboxv1alpha1.SessionHoldList
	require.NoError(t, b.Controller.List(context.Background(), &holds, client.InNamespace("default")))
	assert.Len(t, holds.Items, 2, "a human re-running the command is a second, independent trip, not a retry")
	assert.NotEqual(t, holds.Items[0].Name, holds.Items[1].Name)
}

// TestRunSessionHold_refusedWithoutHoldPermission_createsNoCR is the
// tightening this task adds: a caller who lacks agentsession#hold must be
// refused, AND the refusal must happen before any SessionHold CR is created.
// Asserting only "returns an error" would still pass if the CR were written
// before the (missing) check ran — both halves are asserted here.
func TestRunSessionHold_refusedWithoutHoldPermission_createsNoCR(t *testing.T) {
	installHoldChecker(t, fakeHoldChecker{allowed: false}, nil)
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo-session",
			Namespace: "default",
			UID:       "fixture-uid-1234",
		},
	}
	b := aptest.NewBundle(t, sess)
	g := aptest.GlobalsFor(b)

	err := runSessionHold(context.Background(), io.Discard, g, "demo-session", "looks wrong", "user:mallory")
	require.Error(t, err, "a caller without agentsession#hold must be refused")
	assert.Contains(t, err.Error(), "hold", "the refusal must name the permission")
	assert.Contains(t, err.Error(), "demo-session", "the refusal must name the session")

	var holds spiceboxv1alpha1.SessionHoldList
	require.NoError(t, b.Controller.List(context.Background(), &holds, client.InNamespace("default")))
	assert.Empty(t, holds.Items, "a refused hold must not create a SessionHold CR")
}

// TestRunSessionHold_checkErrorFailsClosed_createsNoCR proves the check fails
// closed: a SpiceDB error (a downed connection, a transient RPC failure)
// refuses the hold rather than letting it through.
func TestRunSessionHold_checkErrorFailsClosed_createsNoCR(t *testing.T) {
	installHoldChecker(t, fakeHoldChecker{err: errors.New("spicedb unavailable")}, nil)
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo-session",
			Namespace: "default",
			UID:       "fixture-uid-1234",
		},
	}
	b := aptest.NewBundle(t, sess)
	g := aptest.GlobalsFor(b)

	err := runSessionHold(context.Background(), io.Discard, g, "demo-session", "looks wrong", "user:mallory")
	require.Error(t, err, "a SpiceDB error must refuse the hold, not proceed")

	var holds spiceboxv1alpha1.SessionHoldList
	require.NoError(t, b.Controller.List(context.Background(), &holds, client.InNamespace("default")))
	assert.Empty(t, holds.Items, "a failed check must not create a SessionHold CR")
}
