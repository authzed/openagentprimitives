package pipeline

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// classWithInteractPolicy builds the AgentClass the fixture Channel points at,
// declaring who may interact with its sessions.
func classWithInteractPolicy(policy string) *spiceboxv1alpha1.AgentClass {
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "ac1", Namespace: "default"},
	}
	if policy != "" {
		ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{
			Session: &spiceboxv1alpha1.SessionAuthz{InteractPermission: policy},
		}
	}
	return ac
}

// twoAuthorThread returns a history page authored by Alice and Bob.
func twoAuthorThread() channelkinds.HistoryPage {
	return channelkinds.HistoryPage{
		Messages: []channelkinds.HistoryMessage{
			{AuthorExternalID: "U_A", AuthorDisplayName: "Alice", AuthorEmail: "a@example.com", Text: "older", TS: "10.1"},
			{AuthorExternalID: "U_B", AuthorDisplayName: "Bob", AuthorEmail: "b@example.com", Text: "newer", TS: "10.2"},
		},
	}
}

func deliverAdoptionMention(t *testing.T, p *Pipeline, ch *spiceboxv1alpha1.Channel) channelkinds.InboundDecision {
	t.Helper()
	dec, err := p.Deliver(context.Background(), channelkinds.InboundEvent{
		Channel:     ch,
		ExternalIDs: channelkinds.ExternalIdentity{Kind: "slack", ExternalID: "U_SUMMONER", Email: "s@example.com"},
		ChannelKey:  "thread:C1:9.9",
		MessageText: "@bot help",
		ThreadEntry: channelkinds.ThreadEntryReply,
		External:    map[string]string{"channel_id": "C1", "thread_ts": "9.9", "message_ts": "11.0"},
	})
	require.NoError(t, err)
	require.Equal(t, channelkinds.OutcomeRouted, dec.Outcome)
	require.True(t, dec.Adopted, "the fixture must take the adoption path")
	return dec
}

// Thread adoption used to grant interact to EVERY distinct thread author, with
// no reference to the class's declared interact policy. A class stating "only
// eng may interact with my sessions" still handed durable standing to whoever
// happened to have posted in the thread first.
//
// That standing is not nominal: interact confers memory_entry#read and
// artifact#view on the session, and once slot grants resolve through it, direct
// permission on external resources.
//
// So when a policy is declared, it governs: an author the policy already admits
// needs no tuple, and one it does not admit gets none.
func TestAdoption_declaredInteractPolicyGovernsWhoIsGranted(t *testing.T) {
	ch := newChannel("c1")
	p, az, _, _, _ := newPipeline(t, ch, classWithInteractPolicy("group:eng#member"))

	aliceCanon := slackCanonical(t, "U_A", "a@example.com").String()
	az.checkInteractFn = func(canonicalID string) (bool, error) {
		// The policy tuple is written before adoption runs, so an admitted
		// author already passes the interact check.
		return canonicalID == aliceCanon, nil
	}
	p.ReadHistory = func(_ context.Context, _ *spiceboxv1alpha1.Channel, _ string, _ channelkinds.ReadHistoryOpts) (channelkinds.HistoryPage, error) {
		return twoAuthorThread(), nil
	}

	dec := deliverAdoptionMention(t, p, ch)

	assert.False(t, az.hasParticipantUser(slackCanonical(t, "U_B", "b@example.com")),
		"Bob is outside the declared policy and must NOT be granted interact by adoption")
	assert.Equal(t, []string{"Alice"}, dec.GrantedParticipants,
		"only the admitted author is announced as able to chat")
	assert.Equal(t, []string{"Bob"}, dec.WithheldParticipants,
		"the refusal must be reported, not silently dropped")
}

// With no policy declared the class has expressed no restriction, so adoption
// keeps its existing behavior. This is what makes the change non-breaking, and
// it is worth pinning: a gate that fired unconditionally would quietly break
// every thread-adoption deployment that never declared a policy.
func TestAdoption_noDeclaredPolicyGrantsEveryThreadAuthor(t *testing.T) {
	ch := newChannel("c1")
	p, az, _, _, _ := newPipeline(t, ch, classWithInteractPolicy(""))
	p.ReadHistory = func(_ context.Context, _ *spiceboxv1alpha1.Channel, _ string, _ channelkinds.ReadHistoryOpts) (channelkinds.HistoryPage, error) {
		return twoAuthorThread(), nil
	}

	dec := deliverAdoptionMention(t, p, ch)

	assert.True(t, az.hasParticipantUser(slackCanonical(t, "U_A", "a@example.com")))
	assert.True(t, az.hasParticipantUser(slackCanonical(t, "U_B", "b@example.com")))
	assert.ElementsMatch(t, []string{"Alice", "Bob"}, dec.GrantedParticipants)
	assert.Empty(t, dec.WithheldParticipants)
}

// A policy that admits nobody must grant nobody. The interesting part is that
// adoption still succeeds — the session exists and the summoner can use it —
// rather than failing the inbound outright.
func TestAdoption_policyAdmittingNobodyGrantsNobody(t *testing.T) {
	ch := newChannel("c1")
	p, az, _, _, _ := newPipeline(t, ch, classWithInteractPolicy("group:eng#member"))
	az.checkInteractFn = func(string) (bool, error) { return false, nil }
	p.ReadHistory = func(_ context.Context, _ *spiceboxv1alpha1.Channel, _ string, _ channelkinds.ReadHistoryOpts) (channelkinds.HistoryPage, error) {
		return twoAuthorThread(), nil
	}

	dec := deliverAdoptionMention(t, p, ch)

	assert.Empty(t, dec.GrantedParticipants)
	assert.ElementsMatch(t, []string{"Alice", "Bob"}, dec.WithheldParticipants)
	assert.False(t, az.hasParticipantUser(slackCanonical(t, "U_A", "a@example.com")))
	assert.False(t, az.hasParticipantUser(slackCanonical(t, "U_B", "b@example.com")))
}

// A failing check must withhold, never grant. Fail-open here would mean a
// SpiceDB blip silently restores the old bulk-grant behavior for exactly the
// classes that took the trouble to declare a policy.
func TestAdoption_checkErrorWithholdsRatherThanGrants(t *testing.T) {
	ch := newChannel("c1")
	p, az, _, _, _ := newPipeline(t, ch, classWithInteractPolicy("group:eng#member"))
	az.checkInteractFn = func(string) (bool, error) { return false, assertAnError }
	p.ReadHistory = func(_ context.Context, _ *spiceboxv1alpha1.Channel, _ string, _ channelkinds.ReadHistoryOpts) (channelkinds.HistoryPage, error) {
		return twoAuthorThread(), nil
	}

	dec := deliverAdoptionMention(t, p, ch)

	assert.Empty(t, dec.GrantedParticipants, "an errored check must not grant")
	assert.ElementsMatch(t, []string{"Alice", "Bob"}, dec.WithheldParticipants)
}

var assertAnError = errAdoptionCheckTest{}

type errAdoptionCheckTest struct{}

func (errAdoptionCheckTest) Error() string { return "spicedb unavailable" }

// The disclosure the channel posts must reflect RESOLVED ownership, not the
// config flag — and the rule that makes those differ is that a starting user
// outranks fromOutputChannel.
//
// A Slack-style inbound always has a starter, so the same Channel config that
// would make a whole channel the owner on an unattributed kind leaves an
// individual owner here. Reporting Collective in this case would tell the room
// they all hold approval authority when one person does.
func TestOwnership_starterOutranksChannelOwnership(t *testing.T) {
	ch := newChannel("c1")
	ch.Spec.Owner = &spiceboxv1alpha1.ChannelOwnerPolicy{
		Ownerless: &spiceboxv1alpha1.ChannelOwnerlessSource{FromOutputChannel: true},
	}
	p, _, _, _, _ := newPipeline(t, ch, classWithInteractPolicy(""))
	p.ReadHistory = func(_ context.Context, _ *spiceboxv1alpha1.Channel, _ string, _ channelkinds.ReadHistoryOpts) (channelkinds.HistoryPage, error) {
		return twoAuthorThread(), nil
	}

	dec := deliverAdoptionMention(t, p, ch)

	assert.False(t, dec.Ownership.Collective,
		"a session with a starting user is owned by that person, not by the channel")
	assert.NotEmpty(t, dec.Ownership.Subject, "ownership must still be resolved and reported")
}
