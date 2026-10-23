package pipeline

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// TestMint_AdmissionSweep covers the channelsd mint sweep, which runs BEFORE
// the Create and only while the session does not exist yet. A minted session
// is pre-stamped with the operator's finalizer, so the operator never sweeps
// it: this sweep is the only one it gets, and it must be fail-closed and
// retry-safe, and must never touch a session that already exists.
func TestMint_AdmissionSweep(t *testing.T) {
	const key = "thread:C1:1"
	plainEvent := func(ch *spiceboxv1alpha1.Channel) channelkinds.InboundEvent {
		return channelkinds.InboundEvent{
			Channel:     ch,
			ExternalIDs: channelkinds.ExternalIdentity{Kind: "fake", ExternalID: "U1", Email: "a@example.com"},
			ChannelKey:  key,
			MessageText: "hello",
		}
	}

	cases := []struct {
		name string
		// channel builds the inbound Channel; nil means newChannel.
		channel func() *spiceboxv1alpha1.Channel
		// existing, when set, pre-creates an object under the minted name —
		// another replica's (or an earlier delivery's) live session. It carries
		// no channel labels, so Deliver's correlating lookup misses it and the
		// fresh-session path runs, exactly as for a replica that lost the race.
		existing   bool
		setup      func(az *fakeAuthz)
		event      func(ch *spiceboxv1alpha1.Channel) channelkinds.InboundEvent
		wantErr    bool
		wantOut    channelkinds.Outcome
		wantSweeps int
		wantData   int
		wantCreate bool
		wantParked bool
	}{
		{
			name:       "fresh mint: both sweeps run, then the session is created pre-stamped with the finalizer",
			event:      plainEvent,
			wantOut:    channelkinds.OutcomeRouted,
			wantSweeps: 1, wantData: 1, wantCreate: true,
		},
		{
			name:       "slot sweep fails: fail closed, InternalError and NO session created",
			setup:      func(az *fakeAuthz) { az.deleteSlotGrantsErr = errors.New("spicedb down") },
			event:      plainEvent,
			wantErr:    true,
			wantOut:    channelkinds.OutcomeInternalError,
			wantSweeps: 1, wantData: 0, wantCreate: false,
		},
		{
			name:       "data-slot sweep fails: fail closed, InternalError and NO session created",
			setup:      func(az *fakeAuthz) { az.deleteDataSlotGrantsErr = errors.New("spicedb down") },
			event:      plainEvent,
			wantErr:    true,
			wantOut:    channelkinds.OutcomeInternalError,
			wantSweeps: 1, wantData: 1, wantCreate: false,
		},
		{
			name:       "session already exists (AlreadyExists race loser / redelivery): no sweep of the live session",
			existing:   true,
			event:      plainEvent,
			wantOut:    channelkinds.OutcomeRouted,
			wantSweeps: 0, wantData: 0, wantCreate: true,
		},
		{
			name:    "parked start: swept before Create like any other mint — the operator will not sweep it",
			channel: func() *spiceboxv1alpha1.Channel { return newOrgScopedChannel("c1") },
			setup: func(az *fakeAuthz) {
				startSessionRefused(az)
				az.lookupSubjectsResult = []string{"YWRtaW5AZXhhbXBsZS5jb20"}
			},
			event: func(ch *spiceboxv1alpha1.Channel) channelkinds.InboundEvent {
				return channelkinds.InboundEvent{Channel: ch, ExternalIDs: guestIdentity("U_GUEST"), ChannelKey: key, MessageText: "hello"}
			},
			wantOut:    channelkinds.OutcomeDeniedByPermission,
			wantSweeps: 1, wantData: 1, wantCreate: true, wantParked: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := newChannel("c1")
			if tc.channel != nil {
				ch = tc.channel()
			}
			objs := []client.Object{ch}
			name := makeSessionName(ch, key)
			if tc.existing {
				objs = append(objs, &spiceboxv1alpha1.AgentSession{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ch.Namespace},
					Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "ac1"},
				})
			}
			p, az, _, _, cli := newPipeline(t, objs...)
			ev := tc.event(ch)
			if tc.setup != nil {
				tc.setup(az)
			}

			dec, err := p.Deliver(context.Background(), ev)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantOut, dec.Outcome)
			assert.Equal(t, tc.wantSweeps, az.deleteSlotGrantsCalls, "DeleteSlotGrants calls")
			assert.Equal(t, tc.wantData, az.deleteDataSlotGrantsCalls, "DeleteDataSlotGrants calls")

			var got spiceboxv1alpha1.AgentSession
			getErr := cli.Get(context.Background(), client.ObjectKey{Namespace: ch.Namespace, Name: name}, &got)
			if !tc.wantCreate {
				assert.True(t, getErr != nil, "no session may exist after a failed sweep")
				return
			}
			require.NoError(t, getErr)
			if !tc.existing {
				assert.Contains(t, got.Finalizers, spiceboxv1alpha1.FinalizerAgentSession,
					"a minted session must carry the operator's finalizer, or the operator's own sweep races the mint-time binds")
			}
			assert.Equal(t, tc.wantParked, spiceboxv1alpha1.StartApprovalPending(&got), "parked")
		})
	}
}
