package slack

import (
	"context"
	"fmt"
	"strings"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// emailApprovers builds n approvers whose canonical is email-typed, so every
// one of them costs a live users.lookupByEmail call to render as a mention.
func emailApprovers(fc *fakeSlackClient, n int) []channelevents.ExternalIdentity {
	if fc.lookupByEmail == nil {
		fc.lookupByEmail = map[string]*slackapi.User{}
	}
	out := make([]channelevents.ExternalIdentity, 0, n)
	for i := range n {
		email := fmt.Sprintf("owner%02d@example.com", i)
		fc.lookupByEmail[email] = &slackapi.User{ID: fmt.Sprintf("U%02d", i)}
		out = append(out, channelevents.ExternalIdentity{
			Kind:      "slack",
			TeamScope: identity.TeamScope("T1"),
			Email:     identity.Email(email),
		})
	}
	return out
}

// TestPublicNoteApproverMentions_RespectsFanoutCap is the regression guard for
// an unbounded fan-out: sendRequest capped DELIVERY at ResolvedApproverFanoutLimit
// but then handed the public-note renderer the FULL, uncapped approver list. On
// a resource with many owners that is one synchronous users.lookupByEmail per
// owner, on channelsd's single delivery goroutine, every time an approval prompt
// is posted — and the ids already resolved during delivery were looked up a
// second time.
//
// The cap is a delivery bound, never an authorization one: an approver who is
// not named in the note can still approve. So naming only the ones actually
// notified is also the honest rendering.
func TestPublicNoteApproverMentions_RespectsFanoutCap(t *testing.T) {
	restore := snapshotCategories(t)
	defer restore()
	channelinteractions.Reset()
	channelinteractions.Register(channelinteractions.Category{
		Name: "permission_request", Tone: channelinteractions.ToneRoutine,
		Deciders:  channelinteractions.DecideOwner,
		Resurface: channelinteractions.ResurfaceNone, Surface: channelinteractions.SurfaceDMOnly,
	})

	const (
		approverCount = 25
		fanoutLimit   = 3
	)
	fc := &fakeSlackClient{}
	s := &interactionSender{
		client:   fc,
		delivery: newInteractionDeliveryStore(),
		deps:     channelkinds.Deps{ApproverFanoutLimit: fanoutLimit},
	}

	p := channelevents.InteractionRequestPayload{
		Category: "permission_request", RequestRef: "r1", Lead: "Session join request",
		Actions: []channelevents.InteractionAction{
			{ID: "approve", Label: "Approve", Style: channelevents.ActionStylePrimary, Kind: channelevents.ActionKindDecision},
		},
		Audience: channelevents.InteractionAudience{
			Scope:          channelevents.AudienceApprovers,
			Approvers:      emailApprovers(fc, approverCount),
			PublicNote:     true,
			PublicNoteBody: "Awaiting approval.",
		},
	}
	env := interactionEnvelope(t, channelevents.KindInteractionRequest, p)

	_, err := s.Send(context.Background(), sessionWithChannel("CHAN", ""), env)
	require.NoError(t, err, "Send")

	assert.LessOrEqual(t, len(fc.lookupByEmailCalls), fanoutLimit,
		"an approval prompt must cost at most one users.lookupByEmail per DELIVERED "+
			"approver: the fan-out cap bounds delivery, and the public note names "+
			"the same set — resolving all %d costs a Slack call each, synchronously, "+
			"on the relay's delivery goroutine", approverCount)

	require.True(t, fc.postedToChannel, "the public note must still post")
	assert.Contains(t, fc.lastPostedText, "Waiting on",
		"the note must still name who the channel is waiting on")
	assert.Equal(t, fanoutLimit, strings.Count(fc.lastPostedText, "<@"),
		"the note names exactly the approvers that were notified")
}
