package pipeline

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"

	// Register the github kind: it is the trigger that can describe itself.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
)

// newGitHubChannel builds the kind=github, role=input Channel a pull-request
// webhook is delivered on. Non-attributable like bento — the PR author has no
// AP identity — so the Channel carries the subject its sessions attribute to.
func newGitHubChannel(name, authzSubject string) *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:           "github",
			Role:           spiceboxv1alpha1.ChannelRoleInput,
			AgentClass:     "ac1",
			AuthzSubject:   authzSubject,
			CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: "s"},
		},
	}
}

// onlySession lists the sessions the fake client holds and returns the single
// one Deliver created.
func onlySession(t *testing.T, p *Pipeline) spiceboxv1alpha1.AgentSession {
	t.Helper()
	var sessions spiceboxv1alpha1.AgentSessionList
	require.NoError(t, p.K8s.List(context.Background(), &sessions))
	require.Len(t, sessions.Items, 1)
	return sessions.Items[0]
}

// TestDeliverStampsTheSessionOpeningForAWebhookTrigger pins the join between
// the two halves of the fix: the pipeline derives the opening line at the same
// place it seeds the outbound anchor, and the outbound relay posts what it
// finds here. Derived here, and only here, because this is where the input
// Channel — the only object that knows whether an inbound carried a human — is
// in hand.
func TestDeliverStampsTheSessionOpeningForAWebhookTrigger(t *testing.T) {
	const subj = "service:demo-reviewer"
	ch := newGitHubChannel("gh-in", subj)
	p, _, _, _, _ := newPipeline(t, ch, newSlackOutputChannel("gh-out", "C_OUT"))

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:      ch,
		ChannelKey:   "pr:demo-org/demo-repo#2",
		MessageText:  "review this pull request",
		AuthzSubject: subj,
	})
	require.NoError(t, err, "Deliver")
	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)

	sess := onlySession(t, p)
	opening := sess.Annotations[spiceboxv1alpha1.AnnotationSessionOpening]
	require.NotEmpty(t, opening, "a webhook-started session must carry the line that opens its thread")
	assert.Contains(t, opening, "demo-org/demo-repo", "the opening names the repository")
	assert.Contains(t, opening, "#2", "the opening names the pull request")
}

// TestDeliverPassesTheDeliveryToTheOpening pins the other half of the join:
// the pipeline hands the verified delivery (DeliveryEvent + RawDelivery, as
// they crossed NATS) to the trigger's kind, so the opening can say what the
// pull request IS — its title, its author — not merely which one it is. A
// pipeline that stopped passing the body would still stamp a valid opening,
// which is why the assertion here is on the enrichment, not on presence.
func TestDeliverPassesTheDeliveryToTheOpening(t *testing.T) {
	const subj = "service:demo-reviewer"
	ch := newGitHubChannel("gh-in", subj)
	p, _, _, _, _ := newPipeline(t, ch, newSlackOutputChannel("gh-out", "C_OUT"))

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:       ch,
		ChannelKey:    "pr:demo-org/demo-repo#2",
		MessageText:   "review this pull request",
		AuthzSubject:  subj,
		DeliveryEvent: "pull_request",
		RawDelivery: []byte(`{
			"action": "opened",
			"number": 2,
			"pull_request": {
				"title": "Add rate limiting",
				"user": {"login": "demo-author"},
				"changed_files": 3
			},
			"repository": {"full_name": "demo-org/demo-repo"}
		}`),
	})
	require.NoError(t, err, "Deliver")
	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)

	sess := onlySession(t, p)
	opening := sess.Annotations[spiceboxv1alpha1.AnnotationSessionOpening]
	assert.Contains(t, opening, `"Add rate limiting"`, "the delivery's title reaches the opening")
	assert.Contains(t, opening, "by demo-author", "the delivery's author reaches the opening")
}

// TestDeliverStampsNoSessionOpeningWhenTheTriggerCannotDescribeItself: bento's
// inbound carries no human either, but the kind offers no sentence for it. The
// session must then look exactly as it does today — no annotation, so the relay
// synthesizes nothing and the thread roots on the agent's first output.
func TestDeliverStampsNoSessionOpeningWhenTheTriggerCannotDescribeItself(t *testing.T) {
	const subj = "service:demo-cron"
	ch := newBentoChannel("cron-in", subj)
	p, _, _, _, _ := newPipeline(t, ch, newSlackOutputChannel("cron-out", "C_OUT"))

	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:      ch,
		ChannelKey:   "cron:cron-in:1.0",
		MessageText:  "run the digest",
		AuthzSubject: subj,
	})
	require.NoError(t, err, "Deliver")
	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)

	sess := onlySession(t, p)
	assert.NotContains(t, sess.Annotations, spiceboxv1alpha1.AnnotationSessionOpening,
		"a kind with no trigger sentence must leave the session untouched")
}
