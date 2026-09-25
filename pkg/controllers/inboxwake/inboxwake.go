// Package inboxwake tells a session's OWN agent something by the only two
// means that work when the agent is not blocked in a tool call: append a fixed
// line to its transcript as an inbound "inbox" turn, then force it awake — the
// same two effects channelsd's applyWake produces for an ordinary human
// message (stamp the wake-requested-at annotation, publish the KindUserMessage
// nudge). Two controllers need exactly this and nothing else: the
// SubagentRequest controller when an attended child ends, and the Workshop
// controller when a person's own test of a built agent starts, pauses, ends
// or times out.
//
// The "inbox" role is the whole reason an out-of-band writer may append here
// at all. The runner is the SOLE writer of "user" and "assistant" turns
// (internal/cmd/channelsd/memory.go states the contract the inbound path
// follows), and turn.Appender keys an entry on (index, role): a line written
// under the "user" role at an index a running loop is holding makes the
// runner's own next append fail ErrIndexConflict and takes the session down.
// Under "inbox" the keys can never coincide, so the append is safe whatever
// the agent is doing at the time — the runner's replay skips inbox turns while
// advancing past them, and its drain promotes each one into a real "user" turn
// at an index it assigns itself.
//
// Nothing here fails its caller's own work: the write that prompted the
// notification (a terminal phase, a status observation) has already landed and
// must not be undone because a line could not be delivered. Every failure is
// logged with the session it concerned AND returned, so a caller that can
// retry — the Workshop controller, which records an event once the line
// landed — decides for itself, and one that cannot still has the log. The
// halves are reported apart (WakeError), because a line that landed and a
// line that never did ask for opposite things: re-attempting the first
// appends it twice, and re-attempting the second is the only way it arrives.
package inboxwake

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
)

// Publish publishes an envelope on a session's IN subject. Both callers wire
// the operator's NATS publisher here; a nil Publish skips the nudge (the
// annotation stamp alone still wakes a reaped session on its next reconcile).
type Publish func(ctx context.Context, ns, name string, env channelevents.Envelope) error

// ErrSessionOver is returned by Notify when the session has finished, so the
// line was NOT appended and no wake was forced. It is the answer that will
// not change: a caller must record the event it was carrying and move on
// rather than re-attempt, which is how a single notice would otherwise turn
// into a retry every few seconds for as long as the caller lives.
var ErrSessionOver = errors.New("inboxwake: the session has finished; the line was not appended")

// WakeError is returned by Notify when the line WAS appended and only the
// wake failed. The distinction is the whole point: the line is durable and in
// the transcript, so a caller must NOT re-attempt it — a second attempt
// appends it a second time — but the agent may not read it until something
// else wakes it (its own next turn, a person's next message, the operator's
// next reconcile of a reaped session). A caller that records delivery records
// it on this error; one that logs, says which half failed.
type WakeError struct{ Err error }

func (e *WakeError) Error() string { return e.Err.Error() }

func (e *WakeError) Unwrap() error { return e.Err }

// sessionOver reports whether a session has reached a phase no runner will
// come back from on its own, so a line appended to it would never be read.
// Three qualify: Failed; Succeeded that was NOT archive-swept (a swept one is
// WakeEligible and resumes on the wake this package forces); and Held, whose
// pod is reaped and which only a human's release can restart.
//
// Every other phase — Running mid-turn included — is appendable, because the
// line rides the "inbox" role and so shares no (index, role) key with
// anything the runner writes.
func sessionOver(sess *v1.AgentSession) bool {
	if v1.WakeEligible(sess) {
		return false
	}
	switch sess.Status.Phase {
	case v1.AgentSessionPhaseSucceeded, v1.AgentSessionPhaseFailed, v1.AgentSessionPhaseHeld:
		return true
	default:
		return false
	}
}

// Notify appends line to (ns, name)'s transcript and forces the session
// awake. It mints its own system approval ("operator:inboxwake") for the
// memory facade's capability door, matching the caller convention every
// other operator-side memory reach in this codebase uses.
//
// The one thing it reads the session for is whether the session is over
// (sessionOver), in which case it returns ErrSessionOver having appended and
// woken nothing. reader is where that Get goes: both callers pass their own
// uncached APIReader, because the phase this decision turns on is the one the
// operator's own informer cache lags on — a cached read can still say Running
// for a session that finished seconds ago, and the line would then be appended
// to a transcript nobody will ever read. A nil reader falls back to c.
//
// What it returns says which half of the work got done, and a caller decides
// what to do from that alone:
//
//   - nil — the line is in the transcript and the agent has been woken.
//   - ErrSessionOver — nothing was appended and nothing was woken, and the
//     answer will not change.
//   - *WakeError — the line IS in the transcript; only the wake failed.
//   - anything else — the line never landed, and no wake was attempted:
//     waking an agent to read something that is not there spawns a runner
//     for nothing.
func Notify(ctx context.Context, c client.Client, reader client.Reader, mem memory.Memory, publish Publish, now func() time.Time, ns, name, line string) error {
	logger := log.FromContext(ctx)
	ref := ns + "/" + name
	ctx = memory.WithSystemApproval(ctx, "operator:inboxwake")
	if reader == nil {
		reader = c
	}

	var sess v1.AgentSession
	if err := reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &sess); err != nil {
		logger.Info("inbox wake: reading the session failed; the line was not appended",
			"session", ref, "line", line, "err", err.Error())
		return fmt.Errorf("get session %s: %w", ref, err)
	}
	if sessionOver(&sess) {
		logger.Info("inbox wake: the session has finished; the line was not appended",
			"session", ref, "line", line, "phase", sess.Status.Phase)
		return ErrSessionOver
	}

	if mem == nil {
		logger.Info("inbox wake: no memory wired; the line was not appended", "session", ref, "line", line)
		return errors.New("no memory wired; the line was not appended")
	}
	if err := AppendNotice(ctx, mem, ns, name, line, now().UTC()); err != nil {
		logger.Info("inbox wake: appending the line failed; the wake is not attempted", "session", ref, "line", line, "err", err.Error())
		return fmt.Errorf("append the line: %w", err)
	}
	if err := ForceWake(ctx, c, publish, now, ns, name); err != nil {
		logger.Info("inbox wake: the line landed but forcing the wake failed; it is not re-appended",
			"session", ref, "line", line, "err", err.Error())
		return &WakeError{Err: fmt.Errorf("force the wake: %w", err)}
	}
	return nil
}

// AppendNotice writes line as an "inbox"-role turn at max(index)+1, the same
// field shape channelsd gives an inbound human message: one text content
// block and a CreatedAt, with Author and Via left empty because no person
// sent this and it came through no view. The runner's drain promotes it into
// a real "user" turn at an index it assigns itself, so the agent's loop reads
// it exactly like any other inbound message it should act on, and an empty
// Author is a no-op for the drain's advanceRequester — the tool-call auth
// subject stays whoever it already was rather than being reset by a line
// nobody sent.
//
// at stamps the turn's CreatedAt: the caller's own injected clock (Notify
// passes now().UTC()), not time.Now(), so the whole Notify call reads one
// wall-clock value and a test can assert on it.
//
// The notice has no index of its own — it claims whatever max+1 is when it is
// written — and channelsd claims the same index for a person's own inbound
// message with no coordination with this package. A lost race is therefore
// ordinary and recoverable: re-read the tail and take the index that is free
// now, bounded at appendNoticeAttempts so a writer taking every index is
// reported rather than retried forever.
func AppendNotice(ctx context.Context, mem memory.Memory, ns, name, line string, at time.Time) error {
	scope := memory.Scope{Kind: "session", ID: ns + "/" + name}
	appender := turn.NewAppender(mem, scope)
	var err error
	for attempt := 1; attempt <= appendNoticeAttempts; attempt++ {
		var nextIndex int
		if nextIndex, err = nextTurnIndex(ctx, mem, scope); err != nil {
			return err
		}
		err = appender.Append(ctx, memory.Turn{
			Index:     nextIndex,
			Role:      "inbox",
			Content:   []memory.ContentBlock{{Type: "text", Text: line}},
			CreatedAt: at,
		})
		if err == nil {
			return nil
		}
		if !indexTaken(err) {
			return err
		}
	}
	return fmt.Errorf("append the notice: the next index was taken on all %d attempts: %w", appendNoticeAttempts, err)
}

// appendNoticeAttempts bounds AppendNotice's re-read-and-retry. Each attempt
// costs one transcript read, and a notice losing three races in a row is not
// a transient race any more — it is a writer this package cannot get ahead
// of, which is a thing to report rather than to keep retrying inside a
// reconcile.
const appendNoticeAttempts = 3

// indexTaken reports whether err says another writer holds the (index, role)
// key this notice reached for. Both shapes are the same lost race, answered
// at the two places it can be caught: turn.Appender's own check-then-put sees
// a competing turn already in the scope (ErrIndexConflict), and the facade's
// append-only guard sees one that landed after that check
// (ErrAppendOnlyConflict).
func indexTaken(err error) bool {
	return errors.Is(err, memory.ErrIndexConflict) || errors.Is(err, memory.ErrAppendOnlyConflict)
}

// nextTurnIndex reads the transcript tail and returns max(index)+1.
func nextTurnIndex(ctx context.Context, mem memory.Memory, scope memory.Scope) (int, error) {
	existing, err := turn.ReadAll(ctx, mem, scope)
	if err != nil {
		return 0, fmt.Errorf("read transcript tail: %w", err)
	}
	next := 0
	for _, t := range existing {
		if t.Index >= next {
			next = t.Index + 1
		}
	}
	return next, nil
}

// ForceWake stamps AnnotationWakeRequestedAt (WakeEligible-gated, so a live
// runner is never double-spawned) and publishes the KindUserMessage nudge.
func ForceWake(ctx context.Context, c client.Client, publish Publish, now func() time.Time, ns, name string) error {
	key := types.NamespacedName{Namespace: ns, Name: name}
	var sess v1.AgentSession
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur v1.AgentSession
		if err := c.Get(ctx, key, &cur); err != nil {
			return err
		}
		sess = cur
		if !v1.WakeEligible(&cur) {
			return nil
		}
		base := cur.DeepCopy()
		if cur.Annotations == nil {
			cur.Annotations = map[string]string{}
		}
		cur.Annotations[v1.AnnotationWakeRequestedAt] = now().UTC().Format(time.RFC3339Nano)
		return c.Patch(ctx, &cur, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	}); err != nil {
		return fmt.Errorf("stamp wake-requested-at: %w", err)
	}
	if publish == nil {
		return nil
	}
	env := channelevents.Envelope{
		Version:     1,
		Kind:        channelevents.KindUserMessage,
		Session:     channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
		PublishedAt: now(),
		Payload:     []byte("{}"),
	}
	if err := env.Validate(); err != nil {
		return fmt.Errorf("build wake envelope: %w", err)
	}
	return publish(ctx, sess.Namespace, sess.Name, env)
}
