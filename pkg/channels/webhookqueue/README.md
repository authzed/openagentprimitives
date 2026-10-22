# Deferred webhook admission

A receiver implementing `channelkinds.DeferredWebhookReceiver` can wait for
provider prerequisites without creating an agent. The HTTP handler authenticates
and translates first, then creates a `WebhookAdmission` before returning 202.
Failure to persist returns 503. Legacy receivers retain their existing NATS
path.

Channelsd polls every 15 seconds. Provider failures retain the original payload
and retry with exponential backoff from 30 seconds to 15 minutes. Admission
status records the latest reason, retry count and lease. A ten-minute status
lease, acquired by resource-version comparison, coordinates replicas; individual
admission calls have a two-minute deadline. The provider returns a normalized
context separately from the signed body, which remains the trigger audit record.

Each immutable provider cohort gets a separate durable claim. A deterministic
session key and the Channel UID annotation recover a crash after session
creation without appending a duplicate turn. Recreated Channels cannot adopt a
predecessor's queue or sessions. Queued records and cohort claims are owned by
the Channel and retained for fourteen days; expired work is logged before
removal. The worker also recognizes existing cohort sessions if a delivery is
replayed later.

Inspect deferrals with:

```sh
kubectl get webhookadmissions -A
kubectl get webhookadmission NAME -n NAMESPACE -o yaml
```

The delivery payload is limited to 512 KiB. The queue contains private webhook
evidence and is accessible only through Kubernetes RBAC; webd can create/get
these records, and channelsd can reconcile their status and retention. Neither
needs general ConfigMap write access. Agent runners receive no queue write
grant.

Polling repairs a missing final completion event once an earlier matching event
has been accepted. If no matching webhook reaches the endpoint at all, the
provider must redeliver a completion event. A missing prerequisite run stays
pending, rather than being interpreted as completed. Queued provider evidence is
revalidated against current Channel configuration and authenticated provider
APIs.
