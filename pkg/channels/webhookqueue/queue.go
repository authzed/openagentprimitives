// Package webhookqueue durably parks verified webhook deliveries before session
// creation. Provider readiness is an optional receiver capability; this package
// never interprets provider payloads or agent policy.
package webhookqueue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	api "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkey"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/go-logr/logr"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const queueLabel = "agentprimitives.authzed.com/webhook-queue"
const channelUIDAnnotation = "agentprimitives.authzed.com/webhook-channel-uid"
const retention = 14 * 24 * time.Hour

// Enqueue must be called only after provider authentication and translation.
// Success means the raw delivery is durable before the HTTP endpoint accepts it.
func Enqueue(ctx context.Context, cli client.Client, ch *api.Channel, event string, body []byte) error {
	if len(body) > 512<<10 {
		return fmt.Errorf("deferred webhook exceeds 512 KiB durable limit")
	}
	identity := string(ch.UID) + "\x00" + event + "\x00" + string(body)
	record := &api.WebhookAdmission{ObjectMeta: meta.ObjectMeta{Namespace: ch.Namespace, Name: objectName("delivery", identity), Labels: map[string]string{queueLabel: "delivery"}, OwnerReferences: []meta.OwnerReference{{APIVersion: api.SchemeGroupVersion.String(), Kind: "Channel", Name: ch.Name, UID: ch.UID}}}, Spec: api.WebhookAdmissionSpec{RecordType: "delivery", ChannelName: ch.Name, ChannelUID: ch.UID, Event: event, Body: body, EnqueuedAt: meta.Now()}}
	if err := cli.Create(ctx, record); err != nil && !kerrors.IsAlreadyExists(err) {
		return fmt.Errorf("persist deferred webhook: %w", err)
	}
	return nil
}
func objectName(kind, key string) string {
	sum := sha256.Sum256([]byte(key))
	return "webhook-" + kind + "-" + hex.EncodeToString(sum[:])[:40]
}

// Queue uses status resource versions as leases across channelsd replicas.
// Cohort claims are separate from delivery records because several independent
// provider completions can become ready for the same immutable revision.
type Queue struct {
	Client  client.Client
	Logger  logr.Logger
	Admit   func(context.Context, *api.Channel, string, []byte) (channelkinds.WebhookAdmission, error)
	Deliver func(context.Context, channelkinds.InboundEvent) error
	Now     func() time.Time
}

func (q *Queue) Run(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		if err := q.Process(ctx); err != nil {
			q.Logger.Info("deferred webhook processing failed", "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (q *Queue) Process(ctx context.Context) error {
	var records api.WebhookAdmissionList
	if err := q.Client.List(ctx, &records); err != nil {
		return err
	}
	var errs []error
	for i := range records.Items {
		record := &records.Items[i]
		if q.now().Sub(record.Spec.EnqueuedAt.Time) > retention {
			q.Logger.Info("deferred webhook retention expired", "namespace", record.Namespace, "record", record.Name, "phase", record.Status.Phase, "reason", record.Status.Reason)
			if err := q.Client.Delete(ctx, record); err != nil && !kerrors.IsNotFound(err) {
				errs = append(errs, err)
			}
			continue
		}
		if record.Spec.RecordType != "delivery" || record.Status.Phase == "done" {
			continue
		}
		claimed, err := q.claim(ctx, record)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !claimed {
			continue
		}
		previousReason := record.Status.Reason
		workCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err = q.processRecord(workCtx, record)
		cancel()
		record.Status.LeaseUntil = meta.Time{}
		if err != nil {
			if record.Status.RetryCount < 32 {
				record.Status.RetryCount++
			}
			delay := 30 * time.Second
			for n := int32(1); n < record.Status.RetryCount && delay < 15*time.Minute; n++ {
				delay *= 2
			}
			if delay > 15*time.Minute {
				delay = 15 * time.Minute
			}
			record.Status.LeaseUntil = meta.NewTime(q.now().Add(delay))
			record.Status.Reason = err.Error()
			errs = append(errs, fmt.Errorf("webhook %s/%s: %w", record.Namespace, record.Name, err))
		}
		if err == nil {
			record.Status.RetryCount = 0
		}
		if previousReason != record.Status.Reason {
			q.Logger.Info("deferred webhook admission", "namespace", record.Namespace, "record", record.Name, "phase", record.Status.Phase, "reason", record.Status.Reason)
		}
		if updateErr := q.Client.Status().Update(ctx, record); updateErr != nil {
			errs = append(errs, updateErr)
		}
	}
	return errors.Join(errs...)
}
func (q *Queue) claim(ctx context.Context, record *api.WebhookAdmission) (bool, error) {
	if !record.Status.LeaseUntil.IsZero() && q.now().Before(record.Status.LeaseUntil.Time) {
		return false, nil
	}
	record.Status.LeaseUntil = meta.NewTime(q.now().Add(10 * time.Minute))
	if err := q.Client.Status().Update(ctx, record); err != nil {
		if kerrors.IsConflict(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
func (q *Queue) processRecord(ctx context.Context, record *api.WebhookAdmission) error {
	var ch api.Channel
	if err := q.Client.Get(ctx, client.ObjectKey{Namespace: record.Namespace, Name: record.Spec.ChannelName}, &ch); err != nil {
		if kerrors.IsNotFound(err) {
			record.Status.Phase = "done"
			record.Status.Reason = "channel deleted"
			return nil
		}
		return err
	}
	if ch.UID != record.Spec.ChannelUID {
		record.Status.Phase = "done"
		record.Status.Reason = "channel replaced"
		return nil
	}
	admit := q.Admit
	if admit == nil {
		admit = q.admit
	}
	admission, err := admit(ctx, &ch, record.Spec.Event, record.Spec.Body)
	if err != nil {
		return err
	}
	for _, ready := range admission.Ready {
		if err := q.deliverCohort(ctx, &ch, record, ready); err != nil {
			return err
		}
	}
	record.Status.Reason = admission.Reason
	if admission.Done {
		record.Status.Phase = "done"
	}
	return nil
}
func (q *Queue) admit(ctx context.Context, ch *api.Channel, event string, body []byte) (channelkinds.WebhookAdmission, error) {
	kind, ok := registry.Get(ch.Spec.Kind)
	if !ok {
		return channelkinds.WebhookAdmission{}, fmt.Errorf("unknown channel kind %q", ch.Spec.Kind)
	}
	receiver, ok := kind.WebhookReceiver(channelkinds.Deps{K8sClient: q.Client}).(channelkinds.DeferredWebhookReceiver)
	if !ok || !receiver.DeferWebhook(ch, event) {
		return channelkinds.WebhookAdmission{Done: true, Reason: "channel no longer defers this event"}, nil
	}
	var secret core.Secret
	if err := q.Client.Get(ctx, client.ObjectKey{Namespace: ch.Namespace, Name: ch.Spec.CredentialsRef.SecretName}, &secret); err != nil {
		return channelkinds.WebhookAdmission{}, err
	}
	return receiver.AdmitWebhook(ctx, ch, channelkinds.WebhookSecrets{Data: secret.Data}, event, body)
}
func (q *Queue) deliverCohort(ctx context.Context, ch *api.Channel, record *api.WebhookAdmission, ready channelkinds.AdmittedWebhook) error {
	claim := &api.WebhookAdmission{ObjectMeta: meta.ObjectMeta{Namespace: ch.Namespace, Name: objectName("cohort", string(ch.UID)+"\x00"+ready.ChannelKey), Labels: map[string]string{queueLabel: "cohort"}, OwnerReferences: record.OwnerReferences}, Spec: api.WebhookAdmissionSpec{RecordType: "cohort", ChannelName: ch.Name, ChannelUID: ch.UID, EnqueuedAt: meta.Now()}}
	if err := q.Client.Create(ctx, claim); err != nil {
		if !kerrors.IsAlreadyExists(err) {
			return err
		}
		if err := q.Client.Get(ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
			return err
		}
	}
	if claim.Status.Phase == "done" {
		return nil
	}
	claimed, err := q.claim(ctx, claim)
	if err != nil {
		return err
	}
	if !claimed {
		return fmt.Errorf("cohort is being admitted by another worker")
	}
	var sessions api.AgentSessionList
	if err := q.Client.List(ctx, &sessions, client.InNamespace(ch.Namespace), client.MatchingLabels{api.LabelChannelName: ch.Name, api.LabelChannelKey: channelkey.LabelValue(ready.ChannelKey)}); err != nil {
		return err
	}
	exists := false
	for _, session := range sessions.Items {
		if session.Annotations[channelUIDAnnotation] == string(ch.UID) {
			exists = true
			break
		}
	}
	if !exists {
		if q.Deliver == nil {
			return fmt.Errorf("deferred webhook delivery is not configured")
		}
		annotations := make(map[string]string, len(ready.PreTurnAnnotations)+1)
		for key, value := range ready.PreTurnAnnotations {
			annotations[key] = value
		}
		annotations[channelUIDAnnotation] = string(ch.UID)
		err = q.Deliver(ctx, channelkinds.InboundEvent{Channel: ch, ChannelKey: ready.ChannelKey, MessageText: ready.MessageText, AuthzSubject: ch.Spec.AuthzSubject, RawDelivery: record.Spec.Body, DeliveryEvent: record.Spec.Event, TriggerContext: ready.Context, TriggerContextEvent: ready.ContextEvent, External: ready.External, PreTurnAnnotations: annotations})
		if err != nil {
			claim.Status.LeaseUntil = meta.Time{}
			if updateErr := q.Client.Status().Update(ctx, claim); updateErr != nil {
				return errors.Join(err, updateErr)
			}
			return err
		}
	}
	claim.Status.Phase = "done"
	claim.Status.LeaseUntil = meta.Time{}
	return q.Client.Status().Update(ctx, claim)
}

func (q *Queue) now() time.Time {
	if q.Now != nil {
		return q.Now()
	}
	return time.Now()
}
