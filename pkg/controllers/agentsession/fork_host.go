package agentsession

import (
	"context"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// forkHost is the operator-side pipeline.Host for the SessionFork point. It is
// constructed per ReconcileRestart call, bound to the parent session + forker.
// v1 supports NO approval (the SessionFork hook emits no ApprovalAsk); Notify
// delivers the requester-facing deny notice to the PARENT session's
// out.metaagent_notice (the child does not exist on deny). Audit/SetStatus/Halt
// log — the authoritative fork decision is also logged + condition-set by the
// reconciler.
//
// Notify ALSO retains the requester-facing text so the reconciler can persist it
// on the RestartDenied condition. The NATS publish is best-effort by design and
// has a history of being silently lost (a raw-JSON payload on a subject the
// wildcard outbound relay also consumes and rejects as "envelope version 0"),
// so the durable condition — not the publish — is what channelsd's session
// watcher relays to the user's thread. The wording still belongs to the hook,
// which is why it is captured here rather than restated by the reconciler.
type forkHost struct {
	d forkHostDeps
	// requesterNotice is the last requester-facing Notice text seen by Notify.
	// Single-goroutine by construction: a forkHost is built per ReconcileRestart
	// call and the executor runs its hooks synchronously on that goroutine.
	requesterNotice string
}

type forkHostDeps struct {
	ParentNs   string
	ParentName string
	Forker     string // canonical "user:<...>" subject
	// NoticePublish publishes {requester, body} to the parent's
	// out.metaagent_notice (wired from internal/cmd/operator/main.go's NATS conn). nil ⇒
	// notice logged + dropped (still fail-closed; delivery is best-effort).
	NoticePublish func(ctx context.Context, parentNs, parentName, requester, body string) error
}

func newForkHost(d forkHostDeps) *forkHost { return &forkHost{d: d} }

func (h *forkHost) PublishApproval(_ context.Context, _ pipeline.ApprovalAsk) (string, error) {
	return "", fmt.Errorf("fork host: approval unsupported in v1")
}

func (h *forkHost) AwaitDecision(_ context.Context, _ string, _ time.Duration) (bool, string, bool, error) {
	return false, "", false, fmt.Errorf("fork host: approval unsupported in v1")
}

func (h *forkHost) Notify(ctx context.Context, n pipeline.Notice) error {
	if n.Text() == "" {
		return nil
	}
	if n.ToRequester {
		h.requesterNotice = n.Text()
	}
	if h.d.NoticePublish == nil {
		log.FromContext(ctx).Info("fork host: Notify (no publish wired; dropped)",
			"session", h.d.ParentNs+"/"+h.d.ParentName, "text", n.Text())
		return nil
	}
	if err := h.d.NoticePublish(ctx, h.d.ParentNs, h.d.ParentName, h.d.Forker, n.Text()); err != nil {
		log.FromContext(ctx).Info("fork host: Notify publish failed",
			"session", h.d.ParentNs+"/"+h.d.ParentName, "err", err.Error())
	}
	return nil
}

func (h *forkHost) SetStatus(ctx context.Context, s pipeline.StatusUpdate) error {
	if s.Text != "" {
		log.FromContext(ctx).Info("fork host: SetStatus (best-effort)",
			"session", h.d.ParentNs+"/"+h.d.ParentName, "text", s.Text)
	}
	return nil
}

func (h *forkHost) Halt(ctx context.Context, reason string) error {
	log.FromContext(ctx).Info("fork host: Halt",
		"session", h.d.ParentNs+"/"+h.d.ParentName, "reason", reason)
	return nil
}

func (h *forkHost) Audit(ctx context.Context, recs []pipeline.AuditRecord) error {
	for _, r := range recs {
		log.FromContext(ctx).Info("fork host: Audit (best-effort)",
			"session", h.d.ParentNs+"/"+h.d.ParentName, "kind", r.Kind, "fields", r.Fields)
	}
	return nil
}

var _ pipeline.Host = (*forkHost)(nil)
