// pkg/controllers/agentclass/userless_input_test.go
//
// Untagged (unit tier): drives the real Reconcile against a fake client and
// asserts the derived status.userlessInput fact — "a session of this class can
// be BORN with no human on it" — is published for other controllers to read.
package agentclass_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentclass"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"

	// The three kinds this table needs registered. github is the
	// non-attributable webhook kind the live defect was found on; slack is the
	// attributable control; agent is non-attributable AND spawns nothing, the
	// combination the predicate's second half exists for. Blank-imported here
	// rather than stubbed so the table asks the REAL Kind.UserAttributable() /
	// Kind.SpawnsSessionOnInbound() answers.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/agent"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// chFor builds a Channel of the given kind/role bound to acName. The spec is
// deliberately thin: the AgentClass reconciler reads only kind + role +
// agentClass off it, and the Channel's own validity is the Channel
// controller's business.
func chFor(t *testing.T, name, acName, kind, role string) *spiceboxv1alpha1.Channel {
	t.Helper()
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:       kind,
			Role:       role,
			AgentClass: acName,
		},
	}
}

// TestReconcile_DerivesUserlessInput pins the property the AgentClass
// publishes for everyone else: at least one bound INBOUND Channel is of a kind
// that cannot attribute its inbound to a human AND can bring a session into
// existence on that inbound.
//
// Four separate rules key off this one fact (interactPermission being
// required — declared or DERIVED from a role=output Channel's membership —
// authzSubject being required, a role=output Channel needing its own
// destination, a pre-agent failure having no audience). Deriving it once here
// is what keeps the fifth from re-deriving the walk in a fifth place, and is
// why BOTH halves of the predicate belong at the derivation: a consumer that
// asks only the first half disagrees with the ones that ask both.
func TestReconcile_DerivesUserlessInput(t *testing.T) {
	scheme := testfixtures.NewScheme(t)

	cases := []struct {
		name string
		// channels is (name suffix, kind, role) per bound Channel.
		channels [][3]string
		// interactPermission on the class; set on rows that must end Valid=True
		// so a Valid class and an invalid one both prove the field is stamped.
		interactPermission string
		want               bool
	}{
		{
			name:     "no bound Channels at all: nothing inbound is userless",
			channels: nil,
			want:     false,
		},
		{
			name:     "slack input (attributable): userlessInput=false",
			channels: [][3]string{{"in", "slack", spiceboxv1alpha1.ChannelRoleInput}},
			want:     false,
		},
		{
			name:               "github input (webhook, no human): userlessInput=true",
			channels:           [][3]string{{"in", "github", spiceboxv1alpha1.ChannelRoleInput}},
			interactPermission: "group:demo-agent-maintainers#member",
			want:               true,
		},
		{
			name:               "github role=both: inbound counts, userlessInput=true",
			channels:           [][3]string{{"both", "github", spiceboxv1alpha1.ChannelRoleBoth}},
			interactPermission: "group:demo-agent-maintainers#member",
			want:               true,
		},
		{
			name:     "github role=output only: produces no inbound, userlessInput=false",
			channels: [][3]string{{"out", "github", spiceboxv1alpha1.ChannelRoleOutput}},
			want:     false,
		},
		{
			name: "slack input alongside a github input: ANY userless input wins",
			channels: [][3]string{
				{"in-slack", "slack", spiceboxv1alpha1.ChannelRoleInput},
				{"in-gh", "github", spiceboxv1alpha1.ChannelRoleInput},
			},
			interactPermission: "group:demo-agent-maintainers#member",
			want:               true,
		},
		{
			// Same shape as the github-input row but with the authz the
			// userless rule demands left out, so the class ends Valid=False.
			// The fact must still be published: a consumer reading it after a
			// refusal must not see a stale/absent answer.
			name:     "github input with the class invalid: still stamped",
			channels: [][3]string{{"in", "github", spiceboxv1alpha1.ChannelRoleInput}},
			want:     true,
		},
		{
			// agent is UserAttributable=false like github, but nothing is ever
			// BORN on it: the SubagentRequest controller pre-creates the child
			// session with its own attribution and its own standing, then binds
			// this Channel to the child's class for the life of the delegation.
			// Counting it would make the fact — and every rule keyed off it —
			// flap on and off per delegation, on a class that changed nothing.
			name:     "agent role=both (a live delegation): spawns nothing, userlessInput=false",
			channels: [][3]string{{"both", "agent", spiceboxv1alpha1.ChannelRoleBoth}},
			want:     false,
		},
		{
			// Same for role=input, so the answer is not an accident of the
			// role=both arm.
			name:     "agent role=input: spawns nothing, userlessInput=false",
			channels: [][3]string{{"in", "agent", spiceboxv1alpha1.ChannelRoleInput}},
			want:     false,
		},
		{
			// The exclusion is per-Channel, not per-class: a class that really
			// does have a userless input keeps the fact while a delegation is
			// live on it.
			name: "github input alongside an agent Channel: the github input still wins",
			channels: [][3]string{
				{"in-gh", "github", spiceboxv1alpha1.ChannelRoleInput},
				{"deleg", "agent", spiceboxv1alpha1.ChannelRoleBoth},
			},
			interactPermission: "group:demo-agent-maintainers#member",
			want:               true,
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			acName := fmt.Sprintf("ac-userless-input-%d", i)

			ac := newClass(acName)
			if tc.interactPermission != "" {
				ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{
					Session: &spiceboxv1alpha1.SessionAuthz{InteractPermission: tc.interactPermission},
				}
			}
			objs := []client.Object{
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
					Data:       map[string][]byte{"api-key": []byte("sk-test")},
				},
				ac,
			}
			for _, ch := range tc.channels {
				objs = append(objs, chFor(t, acName+"-"+ch[0], acName, ch[1], ch[2]))
			}

			c := fakeclient.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objs...).
				WithStatusSubresource(&spiceboxv1alpha1.AgentClass{}).
				Build()
			r := &agentclass.Reconciler{Client: c, APIReader: c}

			_, err := r.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Namespace: "default", Name: acName},
			})
			require.NoError(t, err, "Reconcile must not error")

			var got spiceboxv1alpha1.AgentClass
			require.NoError(t, c.Get(ctx,
				types.NamespacedName{Namespace: "default", Name: acName}, &got),
				"Get AgentClass after reconcile")

			assert.Equal(t, tc.want, got.Status.UserlessInput,
				"status.userlessInput; boundChannels=%+v", got.Status.BoundChannels)
		})
	}
}

// TestReconcile_UserlessInputAndInteractPermissionShareOneWalk pins that the
// published fact and the interactPermission refusal are two readings of the
// SAME derivation: the class that gets refused is exactly the class the field
// marks, and supplying the missing authz clears the refusal WITHOUT clearing
// the fact (the input is still userless — it just now has a subject).
func TestReconcile_UserlessInputAndInteractPermissionShareOneWalk(t *testing.T) {
	scheme := testfixtures.NewScheme(t)

	reconcileWith := func(t *testing.T, acName, interactPermission, authzSubject string) spiceboxv1alpha1.AgentClass {
		t.Helper()
		ctx := context.Background()
		ac := newClass(acName)
		if interactPermission != "" {
			ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{
				Session: &spiceboxv1alpha1.SessionAuthz{InteractPermission: interactPermission},
			}
		}
		ch := chFor(t, acName+"-in", acName, "github", spiceboxv1alpha1.ChannelRoleInput)
		ch.Spec.AuthzSubject = authzSubject

		c := fakeclient.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
					Data:       map[string][]byte{"api-key": []byte("sk-test")},
				},
				ac, ch,
			).
			WithStatusSubresource(&spiceboxv1alpha1.AgentClass{}).
			Build()
		r := &agentclass.Reconciler{Client: c, APIReader: c}
		_, err := r.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: "default", Name: acName},
		})
		require.NoError(t, err, "Reconcile must not error")
		var got spiceboxv1alpha1.AgentClass
		require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: acName}, &got))
		return got
	}

	t.Run("userless input, no interactPermission: userlessInput=true AND Valid=False naming the field", func(t *testing.T) {
		got := reconcileWith(t, "ac-userless-share-refused", "", "service:demo-agent")
		assert.True(t, got.Status.UserlessInput, "the published fact")
		cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
		require.NotNil(t, cond, "Valid condition must be set")
		assert.Equal(t, metav1.ConditionFalse, cond.Status, "msg=%q", cond.Message)
		assert.Equal(t, spiceboxv1alpha1.ReasonAgentClassUserLessMissingAuthz, cond.Reason)
		assert.Contains(t, cond.Message, "interactPermission",
			"the refusal must still name WHICH field is missing")
	})

	t.Run("userless input with the authz supplied: userlessInput stays true, Valid=True", func(t *testing.T) {
		got := reconcileWith(t, "ac-userless-share-ok", "group:demo-agent-maintainers#member", "service:demo-agent")
		assert.True(t, got.Status.UserlessInput,
			"supplying the authz does not make the input attributable")
		cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
		require.NotNil(t, cond, "Valid condition must be set")
		assert.Equal(t, metav1.ConditionTrue, cond.Status, "msg=%q", cond.Message)
	})
}

// TestReconcile_LiveDelegationDoesNotWidenInteract pins the AUTHORIZATION
// consequence of the fact above, on the exact shape a conversational
// delegation produces.
//
// A channel-bound class (Slack input + one Slack role=output) is named in some
// parent's spec.subagents. While a delegation is live the SubagentRequest
// controller binds a kind=agent Channel to THIS class, and that Channel is
// owner-ref'd to the request — so whatever the derived fact does, it does per
// delegation, and undoes when the request is garbage-collected.
//
// The thing that must not happen: with the class declaring no
// spec.authz.session.interactPermission, a userlessInput=true would make the
// reconciler DERIVE one from the role=output Channel's membership, and
// pipeline.TouchInteractParticipant writes that subject-set into SpiceDB as a
// participant tuple on every new session of the class. Everyone in that Slack
// channel would gain interact on sessions of an agent whose own configuration
// never said so, for as long as some parent is delegating.
//
// Asserting on status.derivedSessionInteractPermission rather than on
// userlessInput alone is the point: the bool is the input to several rules, and
// this is the one that writes to SpiceDB.
func TestReconcile_LiveDelegationDoesNotWidenInteract(t *testing.T) {
	scheme := testfixtures.NewScheme(t)
	ctx := context.Background()
	const acName = "ac-delegation-target"

	slackOut := chFor(t, acName+"-out", acName, "slack", spiceboxv1alpha1.ChannelRoleOutput)
	slackOut.Spec.Slack = &spiceboxv1alpha1.SlackChannelConfig{
		OutputDefaults: &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: "C0DEMOOUT"},
	}
	// What the SubagentRequest controller creates: role=both, authzSubject
	// naming the PARENT session, bound to this (the child's) class.
	delegation := chFor(t, acName+"-deleg", acName, "agent", spiceboxv1alpha1.ChannelRoleBoth)
	delegation.Spec.AuthzSubject = "agentsession:default/parent-session"

	c := fakeclient.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "llm-creds", Namespace: "default"},
				Data:       map[string][]byte{"api-key": []byte("sk-test")},
			},
			newClass(acName),
			chFor(t, acName+"-in", acName, "slack", spiceboxv1alpha1.ChannelRoleInput),
			slackOut,
			delegation,
		).
		WithStatusSubresource(&spiceboxv1alpha1.AgentClass{}).
		Build()
	r := &agentclass.Reconciler{Client: c, APIReader: c}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: acName},
	})
	require.NoError(t, err, "Reconcile must not error")

	var got spiceboxv1alpha1.AgentClass
	require.NoError(t, c.Get(ctx,
		types.NamespacedName{Namespace: "default", Name: acName}, &got),
		"Get AgentClass after reconcile")

	assert.False(t, got.Status.UserlessInput,
		"a bound agent Channel spawns no session, so it must not make this class userless-input; boundChannels=%+v",
		got.Status.BoundChannels)
	assert.Empty(t, got.Status.DerivedSessionInteractPermission,
		"no interact policy may be derived from the output Channel's membership: "+
			"this class declared none and its own input names a person")
	assert.Empty(t, got.EffectiveSessionInteractPermission(),
		"the effective policy is what pipeline.TouchInteractParticipant writes to SpiceDB")

	cond := findCondition(got.Status.Conditions, spiceboxv1alpha1.AgentClassConditionValid)
	require.NotNil(t, cond, "Valid condition must be set")
	assert.Equal(t, metav1.ConditionTrue, cond.Status,
		"a live delegation must not invalidate the child's own class; msg=%q", cond.Message)
}
