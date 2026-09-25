// pkg/agent/runner/viewer_interact.go
//
// "May the person I am replying to right now actually use the thing I am
// about to hand them?" — the authorization question an outbound offer of a
// browser page has to answer before it is sent, as opposed to the inbound
// question handleAppToolCallReq answers when a browser calls back in.
package runner

import (
	"context"
	"errors"
	"fmt"
)

// CurrentSpeakerCanInteract reports whether the participant whose message the
// agent is currently answering may interact with ns/name — "would a link to
// this session's own pages actually work for the person I am replying to?".
//
// The subject is lastInboundAuthor, not authSubject: authSubject is written
// only under AuthzCli != nil AND a currentRequester/both tool-auth mode, so
// under startedBy mode it is frozen at whoever started the session, which is
// the wrong person to authorize an offer addressed to whoever is speaking now.
//
// fullyConsistent is true, matching the interact re-check on the browser
// proxy-exec path (handleAppToolCallReq): a participant who joined seconds ago
// is exactly who a handoff is most often addressed to.
//
// Fail-closed, with a distinguishable error per cause: a nil checker, no
// recorded speaker (a session woken by a schedule rather than by a message), a
// non-user speaker subject, and a checker failure all answer (false, err). The
// caller turns any error into a refusal; an indeterminate result must never
// read as permission.
//
// Concurrency: lastInboundAuthor is read here with no lock, and the ordering
// that makes that sound is the dispatch WaitGroup rather than any
// single-goroutine property. The field is written only by the Run goroutine
// (drainInbox, and Run's cold-start turn hydration). A meta tool's Execute
// runs on a per-tool-use goroutine that dispatchToolUses starts and then joins
// with wg.Wait() before Run proceeds, so each write is ordered against every
// such read. The one caller shape that would break this is a goroutine Run
// does NOT join reaching a meta tool: handleAppToolCallReq is that shape, and
// it resolves tools only from Loop.AppTools — which internal/cmd/runner populates from
// synthesized MCP app-tools and never from the meta list — so it cannot. A
// future path that can must take a lock instead of relying on this.
func (l *Loop) CurrentSpeakerCanInteract(ctx context.Context, ia InteractChecker, ns, name string) (bool, error) {
	if ia == nil {
		return false, errors.New("no interact checker is wired for this session")
	}
	speaker := l.lastInboundAuthor
	if speaker.Empty() {
		return false, errors.New("no current speaker is recorded for this session")
	}
	canon, err := speaker.CanonicalUserID()
	if err != nil {
		// Name the subject's TYPE, never the subject itself. The type is the
		// whole of what is actionable ("this speaker is not a person"), while
		// the raw subject is a participant identifier that would then travel
		// wherever this error text travels — and this function does not
		// control that.
		return false, fmt.Errorf("the current speaker is a %q subject rather than a user: %w", speaker.ObjectType(), err)
	}
	if canon.IsZero() {
		// "user:" with nothing after it passes both guards above — it is not
		// Empty() and its object type IS "user" — and would reach SpiceDB as a
		// check with an empty subject id. SpiceDB rejects that as malformed, so
		// the outcome is already a refusal; catching it locally spares the
		// round-trip and keeps the cause legible instead of arriving as a
		// validation error from the other end.
		return false, errors.New("the current speaker has no canonical identity")
	}
	allowed, err := ia.CheckInteract(ctx, ns, name, canon, true)
	if err != nil {
		// The checker's bool is deliberately dropped on this path: a checker
		// that answers (true, err) has decided nothing, and letting its bool
		// through would turn an indeterminate answer into permission.
		return false, fmt.Errorf("the interact check for the current speaker failed: %w", err)
	}
	return allowed, nil
}
