// Package pipelinehost provides the channelsd implementation of pipeline.Host.
//
// channelsd fires the InboundTurn point with the Interact hook (the cheap
// SpiceDB #interact gate), which only ever returns Allow/Deny and never produces
// an ApprovalAsk — so the approval methods return ErrApprovalUnsupported and the
// executor fail-closes that publish error into Halt. Notify/SetStatus/Halt/Audit
// are thin logs: the deny side effects (blocklist, dedup, permission_request
// publish) live in the pipeline's handlePermissionDeny branch, not here.
//
// The package is importable where channelsd's main is not; it imports no cmd/*
// and forms no cycle with the hooks package.
package pipelinehost

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// ErrApprovalUnsupported is returned by PublishApproval/AwaitDecision. The
// channelsd interact gate never asks for approval; a non-nil ApprovalAsk
// reaching this host is a programming error, surfaced (not swallowed) so the
// executor fail-closes into Halt.
var ErrApprovalUnsupported = errors.New("channelsd pipeline host: approval not supported (interact never asks for approval)")

// SessionRef identifies the session for log scoping.
type SessionRef struct {
	Namespace string
	Name      string
}

func (s SessionRef) String() string { return s.Namespace + "/" + s.Name }

// Deps configures the channelsd Host.
type Deps struct {
	Session SessionRef
	Logger  *slog.Logger
}

// Host implements pipeline.Host for channelsd's InboundTurn (interact) point.
type Host struct {
	sess   SessionRef
	logger *slog.Logger
}

// New constructs a channelsd pipeline Host.
func New(d Deps) *Host {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Host{sess: d.Session, logger: logger}
}

// PublishApproval is unsupported for the interact gate.
func (h *Host) PublishApproval(_ context.Context, ask pipeline.ApprovalAsk) (string, error) {
	h.logger.Info("channelsd host: PublishApproval called but unsupported",
		"session", h.sess.String(), "kind", ask.Kind)
	return "", ErrApprovalUnsupported
}

// AwaitDecision is unsupported for the interact gate.
func (h *Host) AwaitDecision(_ context.Context, reqID string, _ time.Duration) (bool, string, bool, error) {
	h.logger.Info("channelsd host: AwaitDecision called but unsupported",
		"session", h.sess.String(), "reqID", reqID)
	return false, "", false, ErrApprovalUnsupported
}

// Notify is a thin log. The interact gate emits no user-facing notices through
// the host; deny messaging is owned by the pipeline's handlePermissionDeny.
func (h *Host) Notify(_ context.Context, n pipeline.Notice) error {
	if n.Text() != "" {
		h.logger.Info("channelsd host: Notify (best-effort, not delivered via host)",
			"session", h.sess.String(), "text", n.Text())
	}
	return nil
}

// SetStatus is a thin log.
func (h *Host) SetStatus(_ context.Context, s pipeline.StatusUpdate) error {
	if s.Text != "" {
		h.logger.Info("channelsd host: SetStatus (best-effort)",
			"session", h.sess.String(), "text", s.Text)
	}
	return nil
}

// Halt is a thin log. The interact gate never Halts (it Denies); a Halt here
// would come from a hook panic, which the pipeline surfaces as
// OutcomeInternalError.
func (h *Host) Halt(_ context.Context, reason string) error {
	h.logger.Info("channelsd host: Halt", "session", h.sess.String(), "reason", reason)
	return nil
}

// Audit is a thin log. The interact gate writes no durable audit through the
// host.
func (h *Host) Audit(_ context.Context, recs []pipeline.AuditRecord) error {
	for _, r := range recs {
		h.logger.Info("channelsd host: Audit (best-effort, not persisted via host)",
			"session", h.sess.String(), "kind", r.Kind)
	}
	return nil
}

var _ pipeline.Host = (*Host)(nil)
