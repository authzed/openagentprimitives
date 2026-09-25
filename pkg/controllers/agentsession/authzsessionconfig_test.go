// pkg/controllers/agentsession/authzsessionconfig_test.go
//
// White-box unit tests for the operator-authored authz_session_config snapshot.
// Uses an in-process memory.Local (the same facade internal/cmd/operator wires) rather
// than envtest, because what is under test is the derivation + the write, not
// the API-server round trip.
package agentsession

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all"
	asc "github.com/authzed/openagentprimitives/pkg/memory/kinds/authz_session_config"
)

// ascScope is the memory scope the snapshot lands in for a session.
func ascScope(sess *spiceboxv1alpha1.AgentSession) memory.Scope {
	return memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name}
}

// ascMemory returns an operator-shaped in-process facade plus a context
// carrying the system approval Reconcile attaches at its top.
func ascMemory(t *testing.T) (memory.Memory, context.Context) {
	t.Helper()
	m := memory.NewLocal(inmem.NewBackend())
	return m, memory.WithSystemApproval(context.Background(), "operator:agentsession-controller")
}

// ascClass builds an AgentClass with the scope block under test.
func ascClass(mutate func(*spiceboxv1alpha1.AgentClass)) *spiceboxv1alpha1.AgentClass {
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "cls", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Authz: &spiceboxv1alpha1.AuthzBlock{
				Scope: &spiceboxv1alpha1.ScopeSpec{
					Enabled:   true,
					ColdStart: "extractAndApprove",
				},
			},
		},
	}
	if mutate != nil {
		mutate(ac)
	}
	return ac
}

// ascSession builds a minimal AgentSession referencing cls.
func ascSession(mutate func(*spiceboxv1alpha1.AgentSession)) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default", UID: "uid-1"},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "cls"},
	}
	if mutate != nil {
		mutate(s)
	}
	return s
}

// TestReconcileAuthzSessionConfig_FreshSession_WritesTheClassDerivedGates is
// the core of the fix: the OPERATOR authors the record authzd derives its
// cold-start policy from, so the gates come from the AgentClass the operator
// read rather than from anything the runner said.
func TestReconcileAuthzSessionConfig_FreshSession_WritesTheClassDerivedGates(t *testing.T) {
	mem, ctx := ascMemory(t)
	r := &Reconciler{LifecycleMemory: mem}

	ac := ascClass(func(c *spiceboxv1alpha1.AgentClass) {
		c.Spec.BoundEntities = []spiceboxv1alpha1.BoundEntityType{{
			ResourceType: "github_repo",
			Description:  "a repository",
			Permission:   "view",
		}}
	})
	sess := ascSession(nil)

	require.NoError(t, r.reconcileAuthzSessionConfig(ctx, sess, ac))

	got, found, err := asc.Get(ctx, mem, ascScope(sess))
	require.NoError(t, err, "read back the snapshot the operator just wrote")
	require.True(t, found, "the operator must author authz_session_config; without it authzd fails the session closed")

	assert.True(t, got.ScopeEnabled, "scopeEnabled mirrors AgentClass.spec.scope.enabled")
	assert.Equal(t, "extractAndApprove", got.ColdStart, "coldStart mirrors AgentClass.spec.scope.coldStart")
	require.Len(t, got.BoundEntities, 1)
	assert.Equal(t, "github_repo", got.BoundEntities[0].ResourceType)
}

// TestReconcileAuthzSessionConfig_UnchangedInputs_IsANoOp pins the idempotency
// bar from AGENTS.md: a re-reconcile with identical inputs must not rewrite the
// record. The accessor stamps Entry.CreatedAt from the wall clock, so "re-Put
// identical content" is NOT good enough — the write has to be skipped.
func TestReconcileAuthzSessionConfig_UnchangedInputs_IsANoOp(t *testing.T) {
	mem, ctx := ascMemory(t)
	counted := &countingPutMemory{Memory: mem}
	r := &Reconciler{LifecycleMemory: counted}

	ac := ascClass(nil)
	sess := ascSession(nil)

	require.NoError(t, r.reconcileAuthzSessionConfig(ctx, sess, ac))
	require.Equal(t, 1, counted.puts, "first reconcile must write the snapshot")

	require.NoError(t, r.reconcileAuthzSessionConfig(ctx, sess, ac))
	assert.Equal(t, 1, counted.puts, "a re-reconcile with unchanged inputs must not rewrite the record")
}

// TestReconcileAuthzSessionConfig_ChangedClass_Rewrites is the other half of
// idempotency: unchanged is a no-op, but a genuine change must land — otherwise
// disabling scope on the class would leave a stale permissive snapshot.
func TestReconcileAuthzSessionConfig_ChangedClass_Rewrites(t *testing.T) {
	mem, ctx := ascMemory(t)
	r := &Reconciler{LifecycleMemory: mem}

	sess := ascSession(nil)
	require.NoError(t, r.reconcileAuthzSessionConfig(ctx, sess, ascClass(nil)))

	off := ascClass(func(c *spiceboxv1alpha1.AgentClass) {
		c.Spec.Authz.Scope.Enabled = false
		c.Spec.Authz.Scope.ColdStart = "off"
	})
	require.NoError(t, r.reconcileAuthzSessionConfig(ctx, sess, off))

	got, found, err := asc.Get(ctx, mem, ascScope(sess))
	require.NoError(t, err)
	require.True(t, found)
	assert.False(t, got.ScopeEnabled, "scope disabled on the class must reach the record")
	assert.Equal(t, "off", got.ColdStart)
}

// TestAuthzSessionSubject pins the acting-principal derivation against the
// runner's Loop.ResolveAuthSubjects, which reads the same two places on the
// same AgentSession. A divergence here silently changes who the extraction
// pipeline attributes a turn to.
func TestAuthzSessionSubject(t *testing.T) {
	const inbound = "canon-current"
	const initiator = "canon-initiator"

	withStartedBy := func(s *spiceboxv1alpha1.AgentSession) {
		s.Annotations = map[string]string{
			spiceboxv1alpha1.AnnotationStartedByCanonicalID: initiator,
		}
	}
	withBoth := func(s *spiceboxv1alpha1.AgentSession) {
		s.Annotations = map[string]string{
			spiceboxv1alpha1.AnnotationStartedByCanonicalID: initiator,
			slack.LastInboundCanonicalIDAnnotationKey:       inbound,
		}
	}

	cases := []struct {
		name   string
		mode   string
		mutate func(*spiceboxv1alpha1.AgentSession)
		want   string
	}{
		{
			name:   "mode unset, no inbound yet (the cold-start state): falls back to the initiator",
			mode:   "",
			mutate: withStartedBy,
			want:   initiator,
		},
		{
			name:   "mode unset, an inbound has landed: the current requester wins",
			mode:   "",
			mutate: withBoth,
			want:   inbound,
		},
		{
			name:   "mode startedBy with an inbound present: still frozen on the initiator",
			mode:   "startedBy",
			mutate: withBoth,
			want:   initiator,
		},
		{
			name:   "mode both with an inbound present: acting principal is the current requester",
			mode:   "both",
			mutate: withBoth,
			want:   inbound,
		},
		{
			name:   "no identity at all (kubectl-driven): empty, which denies at the dispatch site",
			mode:   "",
			mutate: nil,
			want:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ac := ascClass(func(c *spiceboxv1alpha1.AgentClass) {
				c.Spec.Authz.ToolCalls = &spiceboxv1alpha1.ToolCallsAuthz{Subject: tc.mode}
			})
			assert.Equal(t, tc.want, authzSessionSubject(ascSession(tc.mutate), ac).String())
		})
	}
}

// countingPutMemory counts Puts so the no-op assertion can distinguish "wrote
// the same bytes again" from "did not write".
type countingPutMemory struct {
	memory.Memory
	puts int
}

func (c *countingPutMemory) Put(ctx context.Context, e memory.Entry) (memory.Entry, error) {
	c.puts++
	return c.Memory.Put(ctx, e)
}
