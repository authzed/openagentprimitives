// pkg/controllers/agentsession/owner_derivation_test.go
//
// Untagged (unit tier): the operator is the authority on agentsession#owner, so
// it is the writer that has to produce an owner for a session nobody started
// and no Channel declared one for.
//
// The live shape: a freshly installed webhook-driven agent — a github input
// Channel and a Slack output Channel — reached Valid=False with "ownerless
// input requires spec.owner.explicit or spec.owner.ownerless", and was fixed by
// hand with fromOutputChannel: true. The Slack channel id is per-install, so no
// checked-in bundle can carry it; the people in the channel the agent posts
// into are the ones who own what it does there. These tests pin that the
// operator reaches the same subject on its own, and that it stops where a
// declared intent or an admin ceiling says to stop.
package agentsession_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"

	// The kinds the fixtures name. slack is the OwnerGroupProvider the
	// derivation reads through; github is the non-attributable webhook input
	// that leaves the session with no starter. Blank-imported rather than
	// stubbed so the test asks the real Kind answers.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

const derivedOwnerRef = "slack_channel:C0DEMO123#member"

// ghInputChannel is the webhook half of the pairing: a role=input Channel whose
// kind names no starting user. owner carries whatever the case declares (nil for
// the derivation case).
func ghInputChannel(owner *spiceboxv1alpha1.ChannelOwnerPolicy) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent-gh", Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:         "github",
			Role:         spiceboxv1alpha1.ChannelRoleInput,
			AgentClass:   "demo-agent",
			AuthzSubject: "service:demo-agent",
			Owner:        owner,
		},
	}
}

// slackOutputChannel is the half the agent's work is delivered into, and the
// only place a per-install membership subject-set can come from. channelID ""
// omits outputDefaults entirely — the state in which the kind supplies no
// membership at all.
func slackOutputChannel(channelID string) *spiceboxv1alpha1.Channel {
	ch := &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent-slack", Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:       "slack",
			Role:       spiceboxv1alpha1.ChannelRoleOutput,
			AgentClass: "demo-agent",
			Slack:      &spiceboxv1alpha1.SlackChannelConfig{},
		},
	}
	if channelID != "" {
		ch.Spec.Slack.OutputDefaults = &spiceboxv1alpha1.SlackOutputDefaults{ChannelID: channelID}
	}
	return ch
}

// webhookSession is a session created from the github input, bound to the Slack
// output the way channelsd binds one for a role=input Channel. It carries no
// started-by annotation: nobody started it.
func webhookSession() *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-derived-owner", Namespace: "default"},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:         "demo-agent",
			OutputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "demo-agent-slack", Kind: "slack"},
		},
	}
}

func classWithCeiling(ceiling *spiceboxv1alpha1.OwnerCeiling) *spiceboxv1alpha1.AgentClass {
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "default"},
	}
	if ceiling != nil {
		ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{OwnerCeiling: ceiling}
	}
	return ac
}

// TestResolveAndWriteOwners_DerivesFromOutputChannel drives the operator's own
// owner write across every way the derivation can and must not fire.
func TestResolveAndWriteOwners_DerivesFromOutputChannel(t *testing.T) {
	cases := []struct {
		name         string
		owner        *spiceboxv1alpha1.ChannelOwnerPolicy
		ceiling      *spiceboxv1alpha1.OwnerCeiling
		outChannelID string
		wantErr      bool
		wantOwner    string
	}{
		{
			name:         "nothing declared + a resolvable output channel: the channel's members own it",
			outChannelID: "C0DEMO123",
			wantOwner:    derivedOwnerRef,
		},
		{
			name:         "declared fromOutputChannel: same subject, the declaration is honoured",
			owner:        &spiceboxv1alpha1.ChannelOwnerPolicy{Ownerless: &spiceboxv1alpha1.ChannelOwnerlessSource{FromOutputChannel: true}},
			outChannelID: "C0DEMO123",
			wantOwner:    derivedOwnerRef,
		},
		{
			// The regression direction. An operator who named an owner keeps it
			// even though a channel membership was available to derive.
			name:         "declared explicit owner: untouched by the derivation",
			owner:        &spiceboxv1alpha1.ChannelOwnerPolicy{Explicit: "group:demo-maintainers#member"},
			outChannelID: "C0DEMO123",
			wantOwner:    "group:demo-maintainers#member",
		},
		{
			name:         "declared ownerless permission: untouched by the derivation",
			owner:        &spiceboxv1alpha1.ChannelOwnerPolicy{Ownerless: &spiceboxv1alpha1.ChannelOwnerlessSource{Permission: "team:reviewers#member"}},
			outChannelID: "C0DEMO123",
			wantOwner:    "team:reviewers#member",
		},
		{
			// Nothing to derive FROM. The output Channel carries no destination,
			// so its kind yields no membership and the fail-closed refusal
			// stands rather than degrading to anything broader.
			name:         "output channel with no channelId: still fails closed, no owner written",
			outChannelID: "",
			wantErr:      true,
		},
		{
			// The admin veto. starterOnly means the owner must be the starting
			// user; this session has none, so it must have no owner rather than
			// a whole channel's worth.
			name:         "ownerCeiling.starterOnly: refuses rather than deriving a channel owner",
			ceiling:      &spiceboxv1alpha1.OwnerCeiling{StarterOnly: true},
			outChannelID: "C0DEMO123",
			wantErr:      true,
		},
		{
			// The admin named the owner, so that is the owner written — the
			// pin is not a refusal to resolve, it IS the resolution. This used
			// to fail closed only because the resolver could not see the
			// ceiling: with a Channel declaring no owner policy there was
			// nothing for the config-time refusal to catch, so the pin was
			// silently dropped and the session fell through to the starter (or
			// here, to a derived channel membership).
			name:         "ownerCeiling.fixed: writes the pinned owner, whatever the channel would have derived",
			ceiling:      &spiceboxv1alpha1.OwnerCeiling{Fixed: "group:sec#member"},
			outChannelID: "C0DEMO123",
			wantOwner:    "group:sec#member",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inputCh := ghInputChannel(tc.owner)
			ac := classWithCeiling(tc.ceiling)
			sess := webhookSession()

			c := fake.NewClientBuilder().
				WithScheme(testfixtures.NewScheme(t)).
				WithObjects(inputCh, slackOutputChannel(tc.outChannelID), ac, sess).
				Build()
			g := &ownerRecordingGranter{}
			r := &agentsession.Reconciler{Client: c, AuthzGranter: g}

			err := r.ResolveAndWriteOwners(context.Background(), sess, inputCh, ac)
			if tc.wantErr {
				require.Error(t, err, "an unresolvable owner must fail closed")
				assert.Empty(t, g.owners, "no owner tuple may be written when resolution failed")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, []string{tc.wantOwner}, g.owners, "agentsession#owner subject")
		})
	}
}

// TestResolveAndWriteOwners_AddsTriggerOwnerFromAnnotation pins the ADDITIVE
// half of the owner write: a session channelsd annotated with a verified
// trigger owner (the PR author, as github_user:<id>#user) gets that subject
// touched as an owner IN ADDITION to the policy-resolved one — never instead
// of it, because the policy owner is the approval anchor a session must always
// have, and the trigger owner may be an unlinked (empty) subject-set.
func TestResolveAndWriteOwners_AddsTriggerOwnerFromAnnotation(t *testing.T) {
	inputCh := ghInputChannel(nil)
	ac := classWithCeiling(nil)
	sess := webhookSession()
	sess.Annotations = map[string]string{
		spiceboxv1alpha1.AnnotationTriggerOwnerSubject: "github_user:4172237#user",
	}

	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(inputCh, slackOutputChannel("C0DEMO123"), ac, sess).
		Build()
	g := &ownerRecordingGranter{}
	r := &agentsession.Reconciler{Client: c, AuthzGranter: g}

	require.NoError(t, r.ResolveAndWriteOwners(context.Background(), sess, inputCh, ac))
	assert.ElementsMatch(t, []string{derivedOwnerRef, "github_user:4172237#user"}, g.owners,
		"the policy-resolved owner AND the trigger's annotated owner, both")
}

// TestResolveAndWriteOwners_TriggerOwnerValidatesAgainstSessionLinks is the
// fail-closed gate on the annotation: an owner tuple is an authorization
// write, and the annotation is only as trustworthy as the writers holding
// session-patch access — so the resolver re-validates that the value is a
// well-formed subject-set whose type#relation a registered channel kind
// actually links into agentsession (chregistry.SessionRelationLinks). Anything
// else is logged and NOT written; the policy owner still lands.
func TestResolveAndWriteOwners_TriggerOwnerValidatesAgainstSessionLinks(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{name: "a type no channel kind links", value: "hubspot_owner:7#user"},
		{name: "a bare user subject (not a subject-set)", value: "user:demo-attacker"},
		{name: "a linked type with the wrong relation", value: "github_user:7#member"},
		{name: "a wildcard id", value: "github_user:*#user"},
		{name: "an empty id", value: "github_user:#user"},
		{name: "not a subject reference at all", value: "not-a-subject"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inputCh := ghInputChannel(nil)
			ac := classWithCeiling(nil)
			sess := webhookSession()
			sess.Annotations = map[string]string{spiceboxv1alpha1.AnnotationTriggerOwnerSubject: tc.value}

			c := fake.NewClientBuilder().
				WithScheme(testfixtures.NewScheme(t)).
				WithObjects(inputCh, slackOutputChannel("C0DEMO123"), ac, sess).
				Build()
			g := &ownerRecordingGranter{}
			r := &agentsession.Reconciler{Client: c, AuthzGranter: g}

			require.NoError(t, r.ResolveAndWriteOwners(context.Background(), sess, inputCh, ac),
				"a refused annotation must not fail the resolve; the policy owner still stands")
			assert.Equal(t, []string{derivedOwnerRef}, g.owners,
				"only the policy-resolved owner; the annotated subject is refused")
		})
	}
}

// A trigger-owner write that SpiceDB refuses — the live schema not yet
// composing github_user#user during a rolling upgrade is the real case — must
// not surface as a resolve error: the policy owner already landed, and the
// caller's "session left ownerless" log would be false. The failure is logged
// by touchTriggerOwner itself and healed by the next reconcile's TOUCH.
func TestResolveAndWriteOwners_TriggerOwnerWriteFailureIsNotAResolveFailure(t *testing.T) {
	inputCh := ghInputChannel(nil)
	ac := classWithCeiling(nil)
	sess := webhookSession()
	sess.Annotations = map[string]string{
		spiceboxv1alpha1.AnnotationTriggerOwnerSubject: "github_user:4172237#user",
	}

	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(inputCh, slackOutputChannel("C0DEMO123"), ac, sess).
		Build()
	g := &ownerRecordingGranter{failOwnerSubject: "github_user:4172237#user"}
	r := &agentsession.Reconciler{Client: c, AuthzGranter: g}

	require.NoError(t, r.ResolveAndWriteOwners(context.Background(), sess, inputCh, ac),
		"the policy owner landed; a refused ADDITIONAL grant is logged and retried, not a resolve failure")
	assert.Equal(t, []string{derivedOwnerRef}, g.owners,
		"the policy owner tuple must be recorded; the failed trigger-owner write must not be")
}

// A session whose POLICY owner cannot resolve stays wholly ownerless even when
// a trigger-owner annotation is present: the trigger owner may be an unlinked,
// empty subject-set, and writing it as the session's ONLY owner would leave a
// session that looks owned and has no resolvable approver.
func TestResolveAndWriteOwners_TriggerOwnerNeverStandsAlone(t *testing.T) {
	inputCh := ghInputChannel(nil)
	ac := classWithCeiling(nil)
	sess := webhookSession()
	sess.Annotations = map[string]string{
		spiceboxv1alpha1.AnnotationTriggerOwnerSubject: "github_user:4172237#user",
	}

	// Output channel with no channelId: the policy derivation fails closed.
	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(inputCh, slackOutputChannel(""), ac, sess).
		Build()
	g := &ownerRecordingGranter{}
	r := &agentsession.Reconciler{Client: c, AuthzGranter: g}

	require.Error(t, r.ResolveAndWriteOwners(context.Background(), sess, inputCh, ac))
	assert.Empty(t, g.owners, "no owner at all rather than a maybe-empty subject-set as the only one")
}

// TestResolveAndWriteOwners_StarterOutranksTheDerivation is the over-derivation
// control, and it is the one that keeps the derivation narrow: a session a human
// started belongs to that human, not to everyone in the room the reply lands in.
func TestResolveAndWriteOwners_StarterOutranksTheDerivation(t *testing.T) {
	inputCh := ghInputChannel(nil)
	ac := classWithCeiling(nil)
	sess := webhookSession()
	sess.Annotations = map[string]string{
		spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:demo-starter",
	}

	c := fake.NewClientBuilder().
		WithScheme(testfixtures.NewScheme(t)).
		WithObjects(inputCh, slackOutputChannel("C0DEMO123"), ac, sess).
		Build()
	g := &ownerRecordingGranter{}
	r := &agentsession.Reconciler{Client: c, AuthzGranter: g}

	require.NoError(t, r.ResolveAndWriteOwners(context.Background(), sess, inputCh, ac))
	assert.Equal(t, []string{"user:demo-starter"}, g.owners,
		"a starting user owns the session; the derivation is a last resort, not a widening")
}
