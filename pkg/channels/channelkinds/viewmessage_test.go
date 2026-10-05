package channelkinds

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/notice"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

func TestDecisionWireRoundTrip(t *testing.T) {
	in := InboundDecision{
		Outcome: OutcomeRouted,
		Notice: notice.New("session_continued_inherited", notice.Args{
			Lead:     "Continuing in a new session",
			Audience: channelevents.InteractionAudience{Scope: channelevents.AudienceParticipants},
		}),
		NewSession:           true,
		RequesterCanonicalID: "YWxpY2U",
	}
	out, ok := DecisionFromWire(DecisionToWire(in))
	require.True(t, ok, "a decision we produced must round-trip through its own wire form")
	assert.Equal(t, in.Outcome, out.Outcome)
	// The rich notice flattens to a wire form and back, so compare the copy
	// that actually crosses — not the pointer, which is deliberately not
	// round-trippable (see notice.FromWire).
	assert.Equal(t, in.Notice.Args().Lead, out.Notice.Args().Lead)
	assert.Equal(t, in.NewSession, out.NewSession)
	assert.Equal(t, in.RequesterCanonicalID, out.RequesterCanonicalID)
}

// Every declared Outcome must survive DecisionToWire → DecisionFromWire. This
// is what catches a new Outcome member that was added to the enum but not to
// outcomeNames — its label would be "unknown" and the round-trip would land on
// OutcomeUnknown instead of itself. Bounded by outcomeSentinel so a newly
// appended member cannot slip past the loop.
func TestEveryOutcomeRoundTripsThroughTheWire(t *testing.T) {
	for o := OutcomeUnknown; o < outcomeSentinel; o++ {
		out, ok := DecisionFromWire(DecisionToWire(InboundDecision{Outcome: o}))
		require.True(t, ok, "Outcome(%d) produced an unparseable wire label", int(o))
		assert.Equal(t, o, out.Outcome, "Outcome(%d) did not round-trip", int(o))
	}
}

func TestRequestViewMessageSendsPayloadAndDecodesDecision(t *testing.T) {
	var gotEnv channelevents.Envelope
	deps := Deps{
		NATSRequest: func(subject string, data []byte, _ time.Duration) ([]byte, error) {
			assert.Equal(t, "ap.session.ns.sess.in.view_message", subject)
			require.NoError(t, json.Unmarshal(data, &gotEnv))
			return json.Marshal(channelevents.ViewMessageResultPayload{
				Outcome: OutcomeRouted.String(), RequesterCanonicalID: "YWxpY2U",
			})
		},
	}

	dec, err := RequestViewMessage(deps, "ns", "sess", "hello", "",
		ExternalIdentity{Kind: "idp", ExternalID: "a@example.com", Email: "a@example.com"}, false, "")
	require.NoError(t, err)
	assert.Equal(t, OutcomeRouted, dec.Outcome)
	assert.Equal(t, "YWxpY2U", dec.RequesterCanonicalID)

	var pl channelevents.ViewMessagePayload
	require.NoError(t, json.Unmarshal(gotEnv.Payload, &pl))
	assert.Equal(t, "hello", pl.Text)
	assert.Equal(t, "a@example.com", pl.Author.Email.String())
}

// A handler-side failure arrives in the reply's Error field. It must become a
// Go error, not a silently-successful OutcomeRouted.
func TestRequestViewMessageSurfacesHandlerError(t *testing.T) {
	deps := Deps{
		NATSRequest: func(string, []byte, time.Duration) ([]byte, error) {
			return json.Marshal(channelevents.ViewMessageResultPayload{
				Outcome: OutcomeInternalError.String(), Error: "get session: not found",
			})
		},
	}
	dec, err := RequestViewMessage(deps, "ns", "sess", "hi", "", ExternalIdentity{}, false, "")
	require.Error(t, err, "a handler-side error must surface, never read as success")
	assert.Contains(t, err.Error(), "get session: not found")
	assert.Equal(t, OutcomeInternalError, dec.Outcome)
}

// A wire label channelsd sent that we cannot parse (version skew, corruption)
// must be a loud error. Silently decoding it to OutcomeUnknown would let a
// caller's `dec.Outcome == OutcomeRouted` read false while nobody learns why;
// worse, a future mis-parse landing on OutcomeRouted would render a denial as
// a successful send.
func TestRequestViewMessageRejectsUnparseableOutcome(t *testing.T) {
	deps := Deps{
		NATSRequest: func(string, []byte, time.Duration) ([]byte, error) {
			return []byte(`{"outcome":"teleported"}`), nil
		},
	}
	dec, err := RequestViewMessage(deps, "ns", "sess", "hi", "", ExternalIdentity{}, false, "")
	require.Error(t, err, "an unrecognized outcome label must never be silently accepted")
	assert.Contains(t, err.Error(), "teleported")
	assert.Equal(t, OutcomeUnknown, dec.Outcome)
}

// When channelsd reports BOTH a handler-side reason AND an outcome label we
// cannot parse, the reason wins: it is the message an operator can act on.
// This pins the check order in RequestViewMessage — swapping the res.Error and
// !ok branches surfaces the useless "unrecognized outcome" instead.
func TestRequestViewMessageHandlerErrorWinsOverUnparseableOutcome(t *testing.T) {
	deps := Deps{
		NATSRequest: func(string, []byte, time.Duration) ([]byte, error) {
			return []byte(`{"outcome":"teleported","error":"get session: not found"}`), nil
		},
	}
	dec, err := RequestViewMessage(deps, "ns", "sess", "hi", "", ExternalIdentity{}, false, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "get session: not found",
		"the handler's reason is the actionable message; it must not be masked by the label check")
	assert.Equal(t, OutcomeUnknown, dec.Outcome, "an unparseable label still never guesses a real outcome")
}

func TestRequestViewMessageNilRequesterIsALoudWiringError(t *testing.T) {
	_, err := RequestViewMessage(Deps{}, "ns", "sess", "hi", "", ExternalIdentity{}, false, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no NATS request")
}

func TestRequestViewMessageSendsVia(t *testing.T) {
	var gotEnv channelevents.Envelope
	deps := Deps{
		NATSRequest: func(subject string, data []byte, _ time.Duration) ([]byte, error) {
			require.NoError(t, json.Unmarshal(data, &gotEnv))
			return json.Marshal(channelevents.ViewMessageResultPayload{Outcome: OutcomeRouted.String()})
		},
	}
	_, err := RequestViewMessage(deps, "ns", "sess", "hello",
		"urn:ap:view:artifact:artifact-3f2a1b8c",
		ExternalIdentity{Kind: "idp", ExternalID: "a@example.com", Email: "a@example.com"}, false, "")
	require.NoError(t, err)

	var pl channelevents.ViewMessagePayload
	require.NoError(t, json.Unmarshal(gotEnv.Payload, &pl))
	assert.Equal(t, "urn:ap:view:artifact:artifact-3f2a1b8c", pl.Via)
	assert.Equal(t, "hello", pl.Text)
}

func TestRequestViewActorDoesNotDeliverAnotherMessage(t *testing.T) {
	deps := Deps{NATSRequest: func(subject string, data []byte, _ time.Duration) ([]byte, error) {
		var env channelevents.Envelope
		require.NoError(t, json.Unmarshal(data, &env))
		var pl channelevents.ViewMessagePayload
		require.NoError(t, json.Unmarshal(env.Payload, &pl))
		assert.True(t, pl.AttestOnly)
		assert.Empty(t, pl.Text)
		assert.Empty(t, pl.RequestID)
		assert.Equal(t, identity.Email("alice@example.com"), pl.Author.Email)
		return json.Marshal(channelevents.ViewMessageResultPayload{Outcome: "routed"})
	}}
	dec, err := RequestViewActor(deps, "default", "sess", ExternalIdentity{Kind: "idp", Email: "alice@example.com", ExternalID: "alice@example.com"})
	require.NoError(t, err)
	assert.Equal(t, OutcomeRouted, dec.Outcome)
}
