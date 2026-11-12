package runner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// slackSubject is the turn Author channelsd resolves for a Slack sender: the
// email canonical when Slack reported a verified address, else the synthetic
// slack:<team>:<id> one. Built with the production encoder so the test cannot
// drift from what a real turn carries.
func slackSubject(t *testing.T, userID, email string) identity.Subject {
	t.Helper()
	s, err := identity.FromExternal(identity.KindSlack, "T1", identity.RawExternalID(userID), identity.Email(email)).
		AllowSynthetic().Subject()
	require.NoError(t, err, "derive a Slack sender's subject")
	return s
}

// The "Requested by" row names the person ASKING, not the person who started
// the session.
//
// One runner serves a whole multiplayer thread (advanceRequester's doc). When
// Alice starts it and Bob asks for the call, a card reading "Requested by
// @Alice" lends Bob Alice's credibility on a security decision. The requester is
// the current turn's author (lastInboundAuthor), falling back to the session's
// started-by identity only when no speaker is recorded at all. A recorded
// speaker who cannot be named (an agent, a raw passthrough) gets no row: the
// starter did not ask, so naming them would be a guess.
//
// Each row goes from the loop's state, through buildToolCallApprovalAsk, to the
// card the approver reads.
func TestToolApprovalCard_RequesterIsTheCurrentSpeaker(t *testing.T) {
	aliceStarted := map[string]string{
		spiceboxv1alpha1.AnnotationStartedByExternalID: "U_ALICE",
		spiceboxv1alpha1.AnnotationStartedByEmail:      "alice@corp.example",
	}

	cases := []struct {
		name        string
		annotations map[string]string
		speaker     func(t *testing.T) identity.Subject
		// wantValue is the row's plain fallback; "" means no row at all.
		wantValue string
		// wantSubject is the identity Slack resolves the mention from; empty
		// when the row comes from the started-by fallback (no subject).
		wantSubject func(t *testing.T) identity.Subject
	}{
		{
			name:        "shared thread, Alice started it, Bob is asking: the card names Bob",
			annotations: aliceStarted,
			speaker:     func(t *testing.T) identity.Subject { return slackSubject(t, "U_BOB", "bob@corp.example") },
			wantValue:   "bob@corp.example",
			wantSubject: func(t *testing.T) identity.Subject { return slackSubject(t, "U_BOB", "bob@corp.example") },
		},
		{
			name:        "shared thread, Bob has no verified email: the card still names Bob, by Slack id",
			annotations: aliceStarted,
			speaker:     func(t *testing.T) identity.Subject { return slackSubject(t, "U_BOB", "") },
			wantValue:   "U_BOB",
			wantSubject: func(t *testing.T) identity.Subject { return slackSubject(t, "U_BOB", "") },
		},
		{
			name:        "single-participant session: the speaker is the starter, named by email",
			annotations: aliceStarted,
			speaker:     func(t *testing.T) identity.Subject { return slackSubject(t, "U_ALICE", "alice@corp.example") },
			wantValue:   "alice@corp.example",
			wantSubject: func(t *testing.T) identity.Subject { return slackSubject(t, "U_ALICE", "alice@corp.example") },
		},
		{
			name:        "no speaker recorded (woken by a schedule): the started-by pair, named by email",
			annotations: aliceStarted,
			wantValue:   "alice@corp.example",
		},
		{
			name:        "speaker is not a user (an agent-authored turn): no Requested by row, never the starter",
			annotations: aliceStarted,
			speaker:     func(*testing.T) identity.Subject { return "agentsession:ns/child" },
		},
		{
			name:        "speaker is a raw passthrough, not a platform canonical: no Requested by row, never the starter",
			annotations: aliceStarted,
			speaker:     func(*testing.T) identity.Subject { return "user:dana@example.com" },
		},
		{
			name: "no identity at all (kubectl-started): no Requested by row",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := loopWithConsolidatedTimeout(t)
			l.Tools = []tool.Tool{&csFakeTool{name: "do_thing"}}
			l.ChannelKind = "slack"
			l.AgentSession = &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Annotations: tc.annotations}}
			if tc.speaker != nil {
				l.lastInboundAuthor = tc.speaker(t)
			}

			ask, err := l.buildToolCallApprovalAsk(memory.WithSystemApproval(context.Background(), "test"),
				"do_thing", map[string]any{}, authz.Permission{StateImpact: authz.External}, "tu-1", "because",
				identity.CanonicalFromTrusted("", "test fixture"), false)
			require.NoError(t, err, "buildToolCallApprovalAsk")
			require.NotNil(t, ask)

			var row *channelevents.InteractionField
			for _, f := range publishToolCallCard(t, *ask).Fields {
				if f.Label == "Requested by" {
					row = &f
				}
			}
			if tc.wantValue == "" {
				assert.Nil(t, row, "nobody known must mean no row, never a guess")
				return
			}
			require.NotNil(t, row, "the card must name who is asking")
			assert.Equal(t, tc.wantValue, row.Value)
			require.Len(t, row.Mentions, 1)
			assert.Equal(t, identity.KindSlack, row.Mentions[0].Kind, "the session's human channel kind, for the surface to resolve")
			if tc.wantSubject != nil {
				assert.Equal(t, tc.wantSubject(t), row.Mentions[0].Subject,
					"the mention resolves from the speaker's own canonical subject")
			}
		})
	}
}
