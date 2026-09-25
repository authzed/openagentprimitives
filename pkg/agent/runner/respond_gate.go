package runner

// respond_gate.go is the egress adapter wired into meta.RespondConfig.LeakageGate
// (internal/cmd/runner/main.go). It runs the PreResponse pipeline point through the
// per-Loop executor and maps the Outcome back to the error contract that
// meta/respond.go expects:
//
//   - Allow ⇒ nil (publish proceeds).
//   - Deny whose reason carries the InfoLeakAudience [ErrShareDenied] marker ⇒
//     an error WRAPPING leakage.ErrShareDenied, so respond.go's
//     errors.Is(err, leakage.ErrShareDenied) still fires the Terminal/IdleExit
//     yield-to-user behavior (a denied share is terminal, not retryable).
//   - Other Deny ⇒ a plain (retryable) error: a generic block / timeout /
//     misconfiguration the model may recover from.
//   - Halt ⇒ a plain error surfacing the rare session-halt at egress.

import (
	"context"
	"fmt"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz/guardian/leakage"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// shareDeniedMarker is the stable sentinel the InfoLeakAudience hook embeds in
// a PreResponse Deny reason when a previously-denied share is re-attempted (see
// pkg/authz/hooks/infoleakaudience.go). The adapter detects it to map the Deny
// back to leakage.ErrShareDenied. Keep in sync with that hook.
const shareDeniedMarker = "[ErrShareDenied]"

// LeakageGateForRespondAdapter returns the PreResponse egress gate as a func
// value matching meta.RespondConfig.LeakageGate. internal/cmd/runner wires this into the
// RespondConfig closure. Exported so the binary can reference the unexported
// method without a separate package-level indirection.
func (l *Loop) LeakageGateForRespondAdapter() func(ctx context.Context, sess *tool.SessionContext, text string, attachments []channelevents.AttachmentRef) error {
	return l.leakageGateForRespond
}

// leakageGateForRespond runs the PreResponse executor point for the outbound
// reply and maps the verdict to the meta/respond.go error contract.
//
// The audience check measures TEXT. It cannot measure an attachment's bytes,
// so what it needs to know is whether any exist: a reply carrying files is
// partly unmeasured, and the gate drops to the coarse session-wide check
// rather than letting a fully-tagged sentence vouch for an untagged file.
func (l *Loop) leakageGateForRespond(ctx context.Context, sess *tool.SessionContext, text string, attachments []channelevents.AttachmentRef) error {
	host := newRunnerHost(l, hostSession{
		Namespace: sess.Namespace,
		Name:      sess.Name,
		Class:     l.AgentName,
	})
	out, err := l.executor().Run(ctx, pipeline.PreResponse, pipeline.Input{
		Session: pipeline.SessionRef{
			Namespace: sess.Namespace,
			Name:      sess.Name,
			Class:     l.AgentName,
		},
		Requester: l.authSubject,
		Response:  &pipeline.ResponseInfo{Text: text, HasAttachments: len(attachments) > 0},
	}, host)
	if err != nil {
		// A non-nil error is a host-primitive failure the executor could not
		// turn into a verdict; the returned Verdict is the zero value (Allow).
		// Mapping Allow→nil would let the response publish unchecked. Fail
		// CLOSED: return a (retryable) error so respond.go blocks the publish.
		return fmt.Errorf("information-leakage gate could not evaluate response: %w", err)
	}

	switch out.Verdict {
	case pipeline.Allow:
		return nil
	case pipeline.Deny:
		if strings.Contains(out.Reason, shareDeniedMarker) {
			return fmt.Errorf("%s: %w", out.Reason, leakage.ErrShareDenied)
		}
		return fmt.Errorf("information-leakage gate: %s", out.Reason)
	case pipeline.Halt:
		return fmt.Errorf("information-leakage gate halted session: %s", out.Reason)
	}
	return nil
}
