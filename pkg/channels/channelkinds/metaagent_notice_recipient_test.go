package channelkinds

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// metaagent_notice_recipient_test.go pins WHO an out.metaagent_notice is
// addressed to.
//
// The bug: the payload's `requester` field is a Slack user_id (authzd sends
// payload.ApproverID, a "U…"), but the operator's SessionFork deny path sent
// pr.TriggeredBy — a canonical SpiceDB subject, "user:<base64url>". That went
// straight into Slack's PostEphemeral, which cannot address a user by canonical,
// so the deny notice for a refused thread continuation could never be delivered.
//
// This is the SECOND time this exact class landed here: see the "approval flow
// could not DM the approver" incident recorded on resolveSlackUserIDFromCanonical, where a
// canonical was likewise mistaken for a channel-native id. Hence both an
// explicit requesterCanonical field AND a defensive check that refuses to hand a
// "user:"-prefixed subject to Slack verbatim.

// canonicalResolverStub stands in for the live Slack users.lookupByEmail call.
type canonicalResolverStub struct {
	calls []identity.CanonicalUserID
	out   string
	err   error
}

func (r *canonicalResolverStub) resolve(_ context.Context, c identity.CanonicalUserID) (string, error) {
	r.calls = append(r.calls, c)
	return r.out, r.err
}

func TestResolveNoticeRecipient(t *testing.T) {
	const slackID = "U017XJJQD7A"
	// base64url of "dana@example.com" — the canonical form the operator sends.
	const canonical = "ZGFuYUBleGFtcGxlLmNvbQ"

	cases := []struct {
		name        string
		payload     MetaagentNoticePayload
		resolverOut string
		resolverErr error
		want        string
		wantCalls   []identity.CanonicalUserID
		wantErr     bool
	}{
		{
			name:      "channel-native requester (authzd): used as-is, no lookup",
			payload:   MetaagentNoticePayload{Requester: slackID, Body: "hi"},
			want:      slackID,
			wantCalls: nil,
		},
		{
			name:        "requesterCanonical only (operator): resolved to a Slack id",
			payload:     MetaagentNoticePayload{RequesterCanonical: "user:" + canonical, Body: "hi"},
			resolverOut: slackID,
			want:        slackID,
			wantCalls:   []identity.CanonicalUserID{identity.CanonicalFromTrusted("user:"+canonical, "test fixture")},
		},
		{
			// The exact production payload: a canonical subject sitting in the
			// channel-native `requester` field. Must NOT reach Slack verbatim.
			name:        "canonical subject in requester: detected and resolved, never passed through",
			payload:     MetaagentNoticePayload{Requester: "user:" + canonical, Body: "hi"},
			resolverOut: slackID,
			want:        slackID,
			wantCalls:   []identity.CanonicalUserID{identity.CanonicalFromTrusted("user:"+canonical, "test fixture")},
		},
		{
			name:    "requesterCanonical wins when both are present",
			payload: MetaagentNoticePayload{Requester: slackID, RequesterCanonical: "user:" + canonical, Body: "hi"},
			// Explicit beats implicit: a publisher that sets the canonical field
			// means it, so resolve rather than trust a possibly-stale id.
			resolverOut: slackID,
			want:        slackID,
			wantCalls:   []identity.CanonicalUserID{identity.CanonicalFromTrusted("user:"+canonical, "test fixture")},
		},
		{
			name:    "no recipient at all: error, not a blind post",
			payload: MetaagentNoticePayload{Body: "hi"},
			wantErr: true,
		},
		{
			name:        "resolver failure surfaces: never fall back to the raw canonical",
			payload:     MetaagentNoticePayload{RequesterCanonical: "user:" + canonical, Body: "hi"},
			resolverErr: errors.New("slack: users_not_found"),
			wantErr:     true,
			wantCalls:   []identity.CanonicalUserID{identity.CanonicalFromTrusted("user:"+canonical, "test fixture")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := &canonicalResolverStub{out: tc.resolverOut, err: tc.resolverErr}

			got, err := ResolveNoticeRecipient(context.Background(), tc.payload, rr.resolve)

			if tc.wantErr {
				require.Error(t, err)
				assert.Empty(t, got, "no recipient may be returned alongside an error")
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
			}
			assert.Equal(t, tc.wantCalls, rr.calls, "canonical lookups performed")
		})
	}
}

// TestResolveNoticeRecipientNeverReturnsCanonical is the invariant the incident
// reduces to: whatever the payload shape, the returned value is never a
// canonical subject. Handing one to PostEphemeral is what made the deny notice
// undeliverable.
func TestResolveNoticeRecipientNeverReturnsCanonical(t *testing.T) {
	const canonical = "user:ZGFuYUBleGFtcGxlLmNvbQ"
	payloads := []MetaagentNoticePayload{
		{Requester: canonical, Body: "b"},
		{RequesterCanonical: canonical, Body: "b"},
		{Requester: canonical, RequesterCanonical: canonical, Body: "b"},
	}
	for _, pl := range payloads {
		rr := &canonicalResolverStub{out: "U0123ABCD"}
		got, err := ResolveNoticeRecipient(context.Background(), pl, rr.resolve)
		require.NoError(t, err)
		assert.NotEqual(t, canonical, got, "a canonical subject must never be returned as a Slack user_id")
		assert.NotContains(t, got, "user:", "returned recipient must not carry a subject prefix")
	}
}
