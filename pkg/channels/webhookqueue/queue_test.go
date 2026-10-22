package webhookqueue

import (
	"context"
	"fmt"
	"testing"
	"time"

	api "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkey"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestDeferredAdmissionSurvivesRestartAndDeduplicatesCohort(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(scheme))
	require.NoError(t, core.AddToScheme(scheme))
	ch := &api.Channel{ObjectMeta: meta.ObjectMeta{Name: "ci-input", Namespace: "test", UID: "uid1"}}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.WebhookAdmission{}).WithObjects(ch).Build()
	require.NoError(t, Enqueue(ctx, cli, ch, "workflow_run", []byte(`{"id":1}`)))
	ready := false
	delivered := 0
	admit := func(context.Context, *api.Channel, string, []byte) (channelkinds.WebhookAdmission, error) {
		if !ready {
			return channelkinds.WebhookAdmission{Reason: "waiting"}, nil
		}
		return channelkinds.WebhookAdmission{Done: true, Ready: []channelkinds.AdmittedWebhook{{ChannelKey: "revision-attempt-cohort", MessageText: "ready"}}}, nil
	}
	deliver := func(context.Context, channelkinds.InboundEvent) error { delivered++; return nil }
	q := &Queue{Client: cli, Logger: logr.Discard(), Admit: admit, Deliver: deliver}
	require.NoError(t, q.Process(ctx))
	require.Zero(t, delivered)
	// Another process can pick up durable pending state after restart.
	ready = true
	q = &Queue{Client: cli, Logger: logr.Discard(), Admit: admit, Deliver: deliver}
	require.NoError(t, q.Process(ctx))
	require.Equal(t, 1, delivered)
	require.NoError(t, Enqueue(ctx, cli, ch, "workflow_run", []byte(`{"id":2}`)))
	require.NoError(t, q.Process(ctx))
	require.Equal(t, 1, delivered)
}

func TestCohortRecoveryDoesNotAdoptPredecessorChannelSession(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(scheme))
	require.NoError(t, core.AddToScheme(scheme))
	ch := &api.Channel{ObjectMeta: meta.ObjectMeta{Name: "ci-input", Namespace: "test", UID: "replacement-uid"}}
	old := &api.AgentSession{ObjectMeta: meta.ObjectMeta{Name: "old-session", Namespace: "test", Labels: map[string]string{api.LabelChannelName: ch.Name, api.LabelChannelKey: channelkey.LabelValue("same-cohort")}, Annotations: map[string]string{channelUIDAnnotation: "predecessor-uid"}}}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.WebhookAdmission{}).WithObjects(ch, old).Build()
	require.NoError(t, Enqueue(ctx, cli, ch, "workflow_run", []byte(`{"id":1}`)))
	delivered := 0
	q := &Queue{Client: cli, Logger: logr.Discard(), Admit: func(context.Context, *api.Channel, string, []byte) (channelkinds.WebhookAdmission, error) {
		return channelkinds.WebhookAdmission{Done: true, Ready: []channelkinds.AdmittedWebhook{{ChannelKey: "same-cohort", PreTurnAnnotations: map[string]string{channelUIDAnnotation: "provider-cannot-overwrite"}}}}, nil
	}, Deliver: func(_ context.Context, event channelkinds.InboundEvent) error {
		delivered++
		require.Equal(t, string(ch.UID), event.PreTurnAnnotations[channelUIDAnnotation])
		return nil
	}}
	require.NoError(t, q.Process(ctx))
	require.Equal(t, 1, delivered)
}

func TestPendingDeliveryRetriesProviderAndDeliveryFailures(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(scheme))
	require.NoError(t, core.AddToScheme(scheme))
	ch := &api.Channel{ObjectMeta: meta.ObjectMeta{Name: "synthetic-ci", Namespace: "test", UID: "uid1"}}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.WebhookAdmission{}).WithObjects(ch).Build()
	require.NoError(t, Enqueue(ctx, cli, ch, "workflow_run", []byte(`{"id":1}`)))
	now := time.Now().UTC().Truncate(time.Second)
	clock := func() time.Time { return now }
	providerFails, deliveryFails := true, true
	deliveries, providerCalls := 0, 0
	q := &Queue{Client: cli, Logger: logr.Discard(), Now: clock, Admit: func(context.Context, *api.Channel, string, []byte) (channelkinds.WebhookAdmission, error) {
		providerCalls++
		if providerFails {
			return channelkinds.WebhookAdmission{}, fmt.Errorf("provider unavailable")
		}
		return channelkinds.WebhookAdmission{Done: true, Ready: []channelkinds.AdmittedWebhook{{ChannelKey: "cohort"}}}, nil
	}, Deliver: func(context.Context, channelkinds.InboundEvent) error {
		if deliveryFails {
			return fmt.Errorf("session API unavailable")
		}
		deliveries++
		return nil
	}}
	deliveryState := func() api.WebhookAdmissionStatus {
		var records api.WebhookAdmissionList
		require.NoError(t, cli.List(ctx, &records))
		for _, record := range records.Items {
			if record.Spec.RecordType == "delivery" {
				return record.Status
			}
		}
		t.Fatal("delivery record disappeared")
		return api.WebhookAdmissionStatus{}
	}
	require.ErrorContains(t, q.Process(ctx), "provider unavailable")
	state := deliveryState()
	require.EqualValues(t, 1, state.RetryCount)
	require.WithinDuration(t, now.Add(30*time.Second), state.LeaseUntil.Time, time.Second)
	providerFails = false
	require.NoError(t, q.Process(ctx))
	require.Equal(t, 1, providerCalls, "provider must not be polled before retry-not-before")
	now = now.Add(30 * time.Second)
	require.ErrorContains(t, q.Process(ctx), "session API unavailable")
	state = deliveryState()
	require.EqualValues(t, 2, state.RetryCount)
	require.WithinDuration(t, now.Add(time.Minute), state.LeaseUntil.Time, time.Second)
	deliveryFails = false
	// A new worker recovers after the durable retry window without losing work.
	q = &Queue{Client: cli, Logger: logr.Discard(), Now: clock, Admit: q.Admit, Deliver: q.Deliver}
	now = now.Add(59 * time.Second)
	require.NoError(t, q.Process(ctx))
	require.Zero(t, deliveries)
	now = now.Add(time.Second)
	require.NoError(t, q.Process(ctx))
	require.Equal(t, 1, deliveries)
	state = deliveryState()
	require.Zero(t, state.RetryCount)
	require.True(t, state.LeaseUntil.IsZero())
	require.NoError(t, q.Process(ctx))
	require.Equal(t, 1, deliveries)
}

func TestReplacedChannelDeliveryExpiresWithoutProviderCalls(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(scheme))
	require.NoError(t, core.AddToScheme(scheme))
	ch := &api.Channel{ObjectMeta: meta.ObjectMeta{Name: "synthetic-ci", Namespace: "test", UID: "old-uid"}}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.WebhookAdmission{}).WithObjects(ch).Build()
	require.NoError(t, Enqueue(ctx, cli, ch, "workflow_run", []byte(`{"id":1}`)))
	require.NoError(t, cli.Delete(ctx, ch))
	replacement := ch.DeepCopy()
	replacement.ResourceVersion = ""
	replacement.UID = "new-uid"
	require.NoError(t, cli.Create(ctx, replacement))
	q := &Queue{Client: cli, Logger: logr.Discard(), Admit: func(context.Context, *api.Channel, string, []byte) (channelkinds.WebhookAdmission, error) {
		t.Fatal("replaced delivery must not reach provider")
		return channelkinds.WebhookAdmission{}, nil
	}}
	require.NoError(t, q.Process(ctx))
	var records api.WebhookAdmissionList
	require.NoError(t, cli.List(ctx, &records))
	require.Len(t, records.Items, 1)
	require.Equal(t, "done", records.Items[0].Status.Phase)
	require.Equal(t, "channel replaced", records.Items[0].Status.Reason)
}

func TestStaleConcurrentLeaseCannotWin(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(scheme))
	record := &api.WebhookAdmission{ObjectMeta: meta.ObjectMeta{Name: "claim", Namespace: "test"}, Spec: api.WebhookAdmissionSpec{RecordType: "cohort", ChannelName: "synthetic", ChannelUID: "synthetic-uid", EnqueuedAt: meta.Now()}}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.WebhookAdmission{}).WithObjects(record).Build()
	var first, second api.WebhookAdmission
	require.NoError(t, cli.Get(ctx, client.ObjectKeyFromObject(record), &first))
	require.NoError(t, cli.Get(ctx, client.ObjectKeyFromObject(record), &second))
	q := &Queue{Client: cli, Logger: logr.Discard()}
	won, err := q.claim(ctx, &first)
	require.NoError(t, err)
	require.True(t, won)
	won, err = q.claim(ctx, &second)
	require.NoError(t, err)
	require.False(t, won)
}

func TestWorkerRestartRecoversExpiredLease(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, api.AddToScheme(scheme))
	require.NoError(t, core.AddToScheme(scheme))
	now := time.Now().UTC().Truncate(time.Second)
	ch := &api.Channel{ObjectMeta: meta.ObjectMeta{Name: "synthetic-ci", Namespace: "test", UID: "uid1"}}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.WebhookAdmission{}).WithObjects(ch).Build()
	require.NoError(t, Enqueue(ctx, cli, ch, "workflow_run", []byte(`{"id":1}`)))
	var records api.WebhookAdmissionList
	require.NoError(t, cli.List(ctx, &records))
	require.Len(t, records.Items, 1)
	records.Items[0].Status.LeaseUntil = meta.NewTime(now.Add(10 * time.Minute))
	require.NoError(t, cli.Status().Update(ctx, &records.Items[0]))
	delivered := 0
	q := &Queue{Client: cli, Logger: logr.Discard(), Now: func() time.Time { return now }, Admit: func(context.Context, *api.Channel, string, []byte) (channelkinds.WebhookAdmission, error) {
		return channelkinds.WebhookAdmission{Done: true, Ready: []channelkinds.AdmittedWebhook{{ChannelKey: "lease-recovery"}}}, nil
	}, Deliver: func(context.Context, channelkinds.InboundEvent) error { delivered++; return nil }}
	require.NoError(t, q.Process(ctx))
	require.Zero(t, delivered)
	now = now.Add(11 * time.Minute)
	require.NoError(t, q.Process(ctx))
	require.Equal(t, 1, delivered)
}
