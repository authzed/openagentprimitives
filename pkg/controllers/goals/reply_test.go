package goals

import (
	"context"
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"testing"
	"time"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	"github.com/authzed/openagentprimitives/pkg/memory"
	goalsqlite "github.com/authzed/openagentprimitives/pkg/memory/goals/sqlite"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/replydelivery"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	memsqlite "github.com/authzed/openagentprimitives/pkg/memory/sqlite"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type replyFaults struct {
	memory.Memory
	loseAck, failReads, rejectWrite bool
}

func (m *replyFaults) Put(ctx context.Context, e memory.Entry) (memory.Entry, error) {
	if m.rejectWrite {
		return memory.Entry{}, errors.New("reply not accepted")
	}
	stored, err := m.Memory.Put(ctx, e)
	if err == nil && m.loseAck {
		return stored, errors.New("reply acknowledgement lost")
	}
	return stored, err
}
func (m *replyFaults) Query(ctx context.Context, q memory.Query) (memory.QueryResult, error) {
	if m.failReads {
		return memory.QueryResult{}, errors.New("receipt storage unavailable")
	}
	return m.Memory.Query(ctx, q)
}

func TestReplyRecoveryDoesNotResendOrPreventCancellation(t *testing.T) {
	for _, mode := range []string{"lost acknowledgement then cancelled during outage", "cancel before acceptance", "cancel attempted but unaccepted reply"} {
		t.Run(mode, func(t *testing.T) {
			cancelBeforeAttempt := mode == "cancel before acceptance"
			unaccepted := mode == "cancel attempted but unaccepted reply"
			ctx := context.Background()
			db, err := memsqlite.NewClient(filepath.Join(t.TempDir(), "reply.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			require.NoError(t, db.Migrate(ctx))
			store := goalsqlite.New(db.DB())
			require.NoError(t, store.Migrate(ctx))
			now := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
			auth := &runAuthority{}
			svc := &domain.Service{Store: store, Auth: auth, ExecutionAuth: auth, Now: func() time.Time { return now }}
			a := domain.Actor{Domain: domain.Domain{Namespace: "team", Owner: "owner", Class: "assistant", ClassUID: "class-uid"}, Session: "team/source", SessionUID: "source-uid", Proof: "unit-test-human"}
			g, err := svc.Create(ctx, a, domain.CreateRequest{RequestID: "create", Title: "Stretch", Outcome: "Private reminder"})
			require.NoError(t, err)
			g, err = svc.Update(ctx, a, domain.Change{ID: g.ID, Revision: g.Revision, RequestID: "activate", Action: "activate"})
			require.NoError(t, err)
			g, err = svc.RequestExecution(ctx, a, domain.ExecutionRequest{ID: g.ID, Revision: g.Revision, RequestID: "request", Terms: domain.ExecutionTerms{ClassDigest: "class", DueAt: now, ExpiresAt: now.Add(time.Hour), Bounds: domain.ExecutionBounds{DurationSeconds: 300, Turns: 10, Tokens: 10000, ApprovalSeconds: 90}, AllowedOperations: []string{"respond_to_user"}, Evidence: []string{"receipt"}, Destination: domain.PrivateDestination{Channel: "inbox", ChannelUID: "channel-uid", BindingDigest: "binding", Recipient: a.Domain.Owner}}})
			require.NoError(t, err)
			g, err = svc.DecideExecution(ctx, a.Domain, g.ID, domain.ExecutionDecision{RequestID: "unit-decision", Digest: g.Execution.Digest, Owner: a.Domain.Owner, Approved: true, Witness: "unit-only-witness"})
			require.NoError(t, err)
			o, err := store.Schedule(ctx, g)
			require.NoError(t, err)
			binding := &v1.ChannelBinding{Kind: "browser", Name: "inbox", Key: "browser:inbox"}
			source := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "source", UID: "source-uid"}, Spec: v1.AgentSessionSpec{Class: "assistant", InputChannel: binding}}
			root := &v1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: o.SessionName, UID: "root-uid", CreationTimestamp: metav1.NewTime(now), Annotations: map[string]string{v1.AnnotationStartedByCanonicalID: "user:owner"}}, Spec: v1.AgentSessionSpec{Class: "assistant", GoalExecution: ref(o), InputChannel: binding, Budget: &v1.BudgetConfig{MaxTurns: 10, MaxTokens: 10000, MaxDuration: metav1.Duration{Duration: 300 * time.Second}, SessionExpiration: metav1.Duration{Duration: 300 * time.Second}}}, Status: v1.AgentSessionStatus{Phase: v1.AgentSessionPhaseRunning}}
			scheme := runtime.NewScheme()
			require.NoError(t, v1.AddToScheme(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))
			k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(source, root).Build()
			o, err = store.Claim(ctx, domain.ClaimRequest{ID: o.ID, Worker: "setup", Now: now, Lease: time.Second, OwnerLimit: 1, ClassLimit: 1})
			require.NoError(t, err)
			o, err = store.Attach(ctx, o, string(root.UID), now)
			require.NoError(t, err)
			priv := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
			signer := provenance.NewSigner(priv, "system:operator")
			keys := provenance.MapKeyLookup{{Publisher: signer.Publisher(), KeyID: signer.KeyID()}: priv.Public().(ed25519.PublicKey)}
			base := memory.NewLocal(memsqlite.NewBackend(db), memory.WithProvenanceVerifier(provenance.NewWriteVerifier(keys)))
			faults := &replyFaults{Memory: base, loseAck: true, rejectWrite: unaccepted}
			d := &Dispatcher{Service: svc, Store: store, Client: k8s, Reader: k8s, Worker: "first", Now: func() time.Time { return now }, DeliveryMemory: provenance.NewSigningMemory(faults, signer)}
			payload := channelevents.OutboundUserMessagePayload{Text: "Stand up and stretch!"}
			payload.Delivery, err = channelevents.NewDeliveryOperation(string(root.UID), "approved-reply", payload)
			require.NoError(t, err)
			prepared, err := d.PrepareGoalReply(ctx, root, payload, nil)
			require.NoError(t, err)
			require.Equal(t, "prepared", prepared.Reply.State)
			if !cancelBeforeAttempt {
				now = now.Add(2 * time.Second)
				attemptErr := d.Tick(ctx)
				if unaccepted {
					require.ErrorContains(t, attemptErr, "reply not accepted")
				} else {
					require.ErrorContains(t, attemptErr, "reply acknowledgement lost")
				}
				attempted, err := store.Occurrence(ctx, o.ID)
				require.NoError(t, err)
				require.Equal(t, "attempted", attempted.Reply.State)
				faults.failReads = !unaccepted
				faults.rejectWrite, faults.loseAck = false, false
			}
			_, err = svc.Update(ctx, a, domain.Change{ID: g.ID, Revision: g.Revision, RequestID: "cancel", Action: "cancel"})
			require.NoError(t, err)
			now = now.Add(6 * time.Second)
			err = d.Tick(ctx)
			if cancelBeforeAttempt || unaccepted {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "receipt storage unavailable")
			}
			var deleted v1.AgentSession
			require.Error(t, k8s.Get(ctx, client.ObjectKeyFromObject(root), &deleted), "receipt outage must not prevent cancellation")
			now = now.Add(6 * time.Second)
			err = d.Tick(ctx)
			if !cancelBeforeAttempt && !unaccepted {
				require.ErrorContains(t, err, "receipt storage unavailable")
				held, err := store.Occurrence(ctx, o.ID)
				require.NoError(t, err)
				require.Equal(t, domain.OccurrenceUnknown, held.State, "uncertain acceptance keeps capacity reserved")
				faults.failReads, faults.loseAck = false, false
				d.Worker = "restarted"
				d.DeliveryMemory = provenance.NewSigningMemory(base, provenance.NewSigner(priv, "system:operator"))
				now = now.Add(6 * time.Second)
				require.NoError(t, d.Tick(ctx))
			} else {
				require.NoError(t, err)
			}
			final, err := store.Occurrence(ctx, o.ID)
			require.NoError(t, err)
			require.Equal(t, domain.OccurrenceCancelled, final.State)
			readCtx := memory.WithSystemApproval(ctx, "system:operator")
			entries, err := base.Query(readCtx, memory.Query{Scope: memory.Scope{Kind: "session", ID: "team/" + o.SessionName}, Kinds: []string{replydelivery.KindName}})
			require.NoError(t, err)
			if cancelBeforeAttempt {
				require.Empty(t, entries.Entries)
			} else if unaccepted {
				require.Len(t, entries.Entries, 1)
				require.Equal(t, "absent", final.Reply.State)
				require.Nil(t, final.Reply.Receipt)
			} else {
				require.Len(t, entries.Entries, 1, "restart reconciles the original acceptance instead of sending again")
				require.Equal(t, "accepted", final.Reply.State)
				require.NotNil(t, final.Reply.Receipt)
			}
		})
	}
}
