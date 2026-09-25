//go:build integration

// pkg/controllers/agentsession/interactor_write_test.go
//
// Against real SpiceDB, the property a fake Granter cannot prove: the tuple
// ResolveAndWriteOwners writes for a session's human STARTER actually
// satisfies agentclass#can_personalize (LookupPersonalizableClasses' gate,
// Task 3), the status marker dedups across a re-fetch/re-reconcile, and a
// non-human (or absent) starter writes nothing at all.
package agentsession_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/test/testspicedb"
)

// TestInteractorTupleWrittenForHumanStarter pins the whole path: the operator
// writes agentclass#interactor for a session's human started_by subject, the
// write satisfies can_personalize against real SpiceDB, the status marker
// records it exactly once, and a second reconcile (against the object as
// re-fetched from the API, not the in-memory pointer) neither grows the list
// nor re-issues the SpiceDB write.
func TestInteractorTupleWrittenForHumanStarter(t *testing.T) {
	endpoint := testspicedb.Endpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchema(t, endpoint, token)
	spdb, err := spicedb.NewClient(endpoint, token, true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = spdb.Close() })
	ctx := context.Background()

	const ns = "default"
	class := uniqLeak(t, "interactor-class-")
	canonical := identity.CanonicalFromTrusted(uniqLeak(t, "interactor-human-"), "test fixture")
	humanSubject := "user:" + canonical.String()

	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: class, Namespace: ns},
	}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: class + "-sess", Namespace: ns,
			Annotations: map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: humanSubject}},
		Spec: spiceboxv1alpha1.AgentSessionSpec{Class: class},
	}
	c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
		WithObjects(sess, ac).WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
	r := &agentsession.Reconciler{Client: c, AuthzGranter: spdb}

	// First reconcile: resolve owner + write the interactor tuple.
	require.NoError(t, r.ResolveAndWriteOwners(ctx, sess, nil, ac))

	allowed, err := spdb.CheckOnResource(ctx, "agentclass", ns+"/"+class, "can_personalize", canonical, true)
	require.NoError(t, err, "CheckOnResource agentclass#can_personalize")
	assert.True(t, allowed, "the session's human starter must satisfy can_personalize on its class after the tuple write")

	var got1 spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: ns, Name: sess.Name}, &got1))
	assert.Equal(t, []string{humanSubject}, got1.Status.InteractorTuplesWritten,
		"status.interactorTuplesWritten must record the human starter's subject")

	// Second reconcile against the freshly re-fetched object: the dedup marker
	// already carries the subject, so no growth and no re-issued SpiceDB write.
	require.NoError(t, r.ResolveAndWriteOwners(ctx, &got1, nil, ac))
	var got2 spiceboxv1alpha1.AgentSession
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: ns, Name: sess.Name}, &got2))
	assert.Equal(t, []string{humanSubject}, got2.Status.InteractorTuplesWritten,
		"a second reconcile must not grow interactorTuplesWritten")
}

// TestInteractorTupleWrittenForHumanStarter_NonHumanOrAbsentStarter covers the
// two ways a session's starter is not a person: no started_by annotation at
// all (a kubectl-created or otherwise unattributed session, given an explicit
// owner so resolution still succeeds), and a non-user subject-set starter (a
// group, standing in for any starter that resolves an owner without being a
// person — schema-admissible on agentsession#owner unlike an arbitrary
// service: subject, which the base schema refuses outright and would never
// reach owner resolution at all). Neither writes an interactor tuple —
// #interactor is keyed on user: subjects, and neither a bot nor a group
// interacted with the class as a person.
func TestInteractorTupleWrittenForHumanStarter_NonHumanOrAbsentStarter(t *testing.T) {
	endpoint := testspicedb.Endpoint(t)
	token := testspicedb.UniqueToken(t)
	testspicedb.WriteSchema(t, endpoint, token)
	spdb, err := spicedb.NewClient(endpoint, token, true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = spdb.Close() })
	ctx := context.Background()

	const ns = "default"

	cases := []struct {
		name    string
		starter string // "" omits the annotation entirely
		ceiling *spiceboxv1alpha1.ChannelOwnerPolicy
	}{
		{
			name:    "no started_by annotation at all (explicit policy owner resolves instead)",
			starter: "",
			ceiling: &spiceboxv1alpha1.ChannelOwnerPolicy{Explicit: "group:eng#member"},
		},
		{
			name:    "group subject-set starter (not a person; resolves an owner on its own)",
			starter: "group:automation-bots#member",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class := uniqLeak(t, "interactor-nonhuman-class-")
			ac := &spiceboxv1alpha1.AgentClass{ObjectMeta: metav1.ObjectMeta{Name: class, Namespace: ns}}
			sess := &spiceboxv1alpha1.AgentSession{
				ObjectMeta: metav1.ObjectMeta{Name: class + "-sess", Namespace: ns},
				Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: class},
			}
			if tc.starter != "" {
				sess.Annotations = map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: tc.starter}
			}
			var ch *spiceboxv1alpha1.Channel
			if tc.ceiling != nil {
				// Passed directly to ResolveAndWriteOwners below (its policy read is
				// from the pointer, not a client Get), so it needs no fake-client
				// registration.
				ch = &spiceboxv1alpha1.Channel{
					ObjectMeta: metav1.ObjectMeta{Name: class + "-ch", Namespace: ns},
					Spec:       spiceboxv1alpha1.ChannelSpec{Kind: "fake", Role: spiceboxv1alpha1.ChannelRoleInput, AgentClass: class, Owner: tc.ceiling},
				}
			}
			c := fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).
				WithObjects(sess, ac).WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).Build()
			r := &agentsession.Reconciler{Client: c, AuthzGranter: spdb}

			require.NoError(t, r.ResolveAndWriteOwners(ctx, sess, ch, ac))
			assert.Empty(t, sess.Status.InteractorTuplesWritten,
				"a non-human or absent starter must write no interactor tuple")
		})
	}
}
