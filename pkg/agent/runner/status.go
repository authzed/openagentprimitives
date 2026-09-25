package runner

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/completion"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// maxActiveWidgets bounds status.activeWidgets so a long session with many
// MCP-UI widgets doesn't grow the AgentSession object without limit. Oldest
// entries are dropped first (FIFO by append order).
const maxActiveWidgets = 10

// maxRunnerNotes bounds status.runnerNotes the same way, for the same reason.
// Notes are written on exceptional paths only, so a session that reaches this
// many has a story to tell — and the newest entries are the ones that tell it.
const maxRunnerNotes = 20

// Progress mirrors the spec's status.progress shape.
type Progress struct {
	TurnCount           int32
	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64
	ToolCallCount       int32
}

type StatusPatcher struct {
	c   client.Client
	key types.NamespacedName

	// reader serves the reads whose correctness depends on observing this
	// patcher's OWN just-committed writes. Defaults to c; override via
	// WithDirectReader when c reads through an informer cache.
	reader client.Reader

	// local, when true, makes mutate and Get short-circuit to nil. Used by
	// in-process callers (the gen-agent loop) that have no AgentSession CR
	// to patch. localResult captures the AgentResult passed to
	// WriteSucceeded so the caller can read it back via LocalResult();
	// localFailure captures the (reason, message) pair passed to
	// WriteFailed so the caller can surface a non-generic error.
	local        bool
	localResult  *spiceboxv1alpha1.AgentResult
	localFailure *LocalFailure
	// localIdle records that WriteIdle was called: with no CR to patch, the
	// call otherwise leaves no trace, and an in-process caller cannot tell a
	// session that parked cleanly from one that exited with its phase stuck
	// wherever it happened to be.
	localIdle bool
	// localBypasses captures RecordCompletionBypass calls for the same reason
	// localResult captures WriteSucceeded's: with no CR to patch, an in-process
	// caller would otherwise have no way to see that an override happened.
	localBypasses []spiceboxv1alpha1.CompletionBypass

	// outage is the schedule patchWithRetry uses to wait out a server-side
	// outage. A field rather than a constant so in-package tests can shrink it;
	// production wiring never sets it (NewStatusPatcher installs the default).
	outage outageWait
}

// outageWait is how long, and how patiently, a write waits out an API-server
// outage before giving up. See defaultOutageWait for the sizing argument.
type outageWait struct {
	initial time.Duration
	cap     time.Duration
	budget  time.Duration
}

// defaultOutageWait is sized to outlive an operator restart.
//
// The agentsessionidentity webhook is failurePolicy: Fail, matches the
// `-runner-sa` principals, and is served BY THE OPERATOR, which runs replicas: 1
// with strategy: Recreate (its memory PVC is ReadWriteOnce). Every `oap install`,
// image bump, eviction and drain therefore takes the webhook's endpoints to zero
// for a teardown + schedule + image-pull + boot window; two minutes covers that
// with room. The cap keeps a long outage from becoming one sparse poll; the
// jitter keeps every runner in the cluster from retrying in lockstep.
//
// The budget is finite because a runner blocked forever on a status write shows
// no progress and nothing times it out. At expiry the error is returned and the
// runner exits, which is survivable: the operator does not terminalize a session
// for a crash it did not witness (shouldFastFailRunner in the AgentSession
// reconciler), so the pod restarts until the operator is back.
var defaultOutageWait = outageWait{
	initial: time.Second,
	cap:     15 * time.Second,
	budget:  2 * time.Minute,
}

// LocalFailure carries the (reason, message) pair WriteFailed was called
// with, captured by LocalStatusPatcher so in-process callers can surface
// the real failure rather than a generic "loop ended" message.
type LocalFailure struct {
	Reason  string
	Message string
}

// NewStatusPatcher builds a patcher over c. c MUST read its own writes — if it
// reads through an informer cache, call WithDirectReader with an uncached
// reader, or RequestWake will decide from a phase that predates its own
// WriteIdle.
func NewStatusPatcher(c client.Client, key types.NamespacedName) *StatusPatcher {
	return &StatusPatcher{c: c, key: key, reader: c, outage: defaultOutageWait}
}

// WithDirectReader points the read-your-own-writes reads (today: RequestWake's
// eligibility check) at an uncached, straight-to-apiserver reader. Required
// whenever the patcher's client reads through an informer cache: RequestWake
// decides from the phase WriteIdle just wrote, and a cached Get lags its own
// write by a watch round-trip, so it hands back the pre-Idle phase and the wake
// is silently skipped — the exact silent-strand the wake exists to prevent. Same
// seam as the runner factory's APIReader.
//
// A nil reader is ignored rather than installed: it would panic on the first
// Get, while the c-backed default is at worst stale, never fatal.
func (s *StatusPatcher) WithDirectReader(r client.Reader) *StatusPatcher {
	if r != nil {
		s.reader = r
	}
	return s
}

const maxRetries = 5

// PatchProgress updates the live counters. Optimistic-lock retried.
func (s *StatusPatcher) PatchProgress(ctx context.Context, p Progress) error {
	return s.mutate(ctx, func(sess *spiceboxv1alpha1.AgentSession) {
		sess.Status.Progress = &spiceboxv1alpha1.AgentSessionProgress{
			TurnCount:           p.TurnCount,
			InputTokens:         p.InputTokens,
			OutputTokens:        p.OutputTokens,
			CacheCreationTokens: p.CacheCreationTokens,
			CacheReadTokens:     p.CacheReadTokens,
			ToolCallCount:       p.ToolCallCount,
		}
		if sess.Status.Phase == "" || sess.Status.Phase == spiceboxv1alpha1.AgentSessionPhasePending {
			sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseRunning
			now := metav1.Now()
			if sess.Status.StartedAt == nil {
				sess.Status.StartedAt = &now
			}
		}
	})
}

// PatchRunDuration writes the cumulative active run-time to status.runDuration.
// Unlike PatchProgress (which overwrites per-pod counters), this is the
// monotonic session-lifetime accumulator the next pod seeds from. Optimistic-
// lock retried; no-op in local mode.
func (s *StatusPatcher) PatchRunDuration(ctx context.Context, d time.Duration) error {
	return s.mutate(ctx, func(sess *spiceboxv1alpha1.AgentSession) {
		rd := metav1.Duration{Duration: d}
		sess.Status.RunDuration = &rd
	})
}

// PatchToolGuard replaces the live tool-guard snapshot (open breakers).
// Called on breaker transitions only, not per tool call.
func (s *StatusPatcher) PatchToolGuard(ctx context.Context, tg *spiceboxv1alpha1.ToolGuardStatus) error {
	return s.mutate(ctx, func(sess *spiceboxv1alpha1.AgentSession) {
		sess.Status.ToolGuard = tg
	})
}

// PatchEstimatedCost stamps the end-of-session cost estimate. Optimistic-lock
// retried, same mutate path as PatchProgress.
//
// MONOTONIC: a lower amount never replaces a higher one. The cost hook stamps on
// every terminal path (including idle) from the loop's per-POD token counters,
// which restart at zero after an idle→reap→wake cycle; without the guard a woken
// pod that made no LLM calls of its own overwrites a real session cost with
// AmountMicroUSD: 0 and a nil breakdown. The whole prior record is kept (rather
// than max-ing the amount alone) so ByModel stays consistent with the amount it
// explains.
//
// This bounds the damage without making the figure cumulative across pods: a
// woken pod that DOES spend still reports only its own spend. Full accumulation
// needs the loop to seed its usage counters from durable state at boot.
func (s *StatusPatcher) PatchEstimatedCost(ctx context.Context, c spiceboxv1alpha1.EstimatedSessionCost) error {
	return s.mutate(ctx, func(sess *spiceboxv1alpha1.AgentSession) {
		if prev := sess.Status.EstimatedCost; prev != nil && prev.AmountMicroUSD > c.AmountMicroUSD {
			return
		}
		cc := c
		sess.Status.EstimatedCost = &cc
	})
}

// WriteSucceeded writes the terminal success state.
func (s *StatusPatcher) WriteSucceeded(ctx context.Context, r spiceboxv1alpha1.AgentResult) error {
	if s.local {
		// Capture for in-process callers; no CR to patch.
		rr := spiceboxv1alpha1.AgentResult{Summary: r.Summary, Artifacts: r.Artifacts}
		s.localResult = &rr
	}
	return s.mutate(ctx, func(sess *spiceboxv1alpha1.AgentSession) {
		// Idempotent: don't overwrite a prior terminal state.
		if isTerminal(sess.Status.Phase) {
			return
		}
		sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseSucceeded
		sess.Status.Result = &spiceboxv1alpha1.AgentResult{Summary: r.Summary, Artifacts: r.Artifacts}
		now := metav1.Now()
		sess.Status.FinishedAt = &now
		sess.Status.AwaitingUserInputSince = nil
		meta.SetStatusCondition(&sess.Status.Conditions, metav1.Condition{
			Type: spiceboxv1alpha1.AgentSessionConditionSucceeded, Status: metav1.ConditionTrue,
			Reason: spiceboxv1alpha1.ReasonAgentSessionComplete, ObservedGeneration: sess.Generation,
		})
	})
}

// RecordCompletionBypass appends one completion-requirement override to status
// and returns its 1-based ordinal within the session.
//
// Appends rather than replaces: a second bypass in the same session is a
// separate event, and "it happened twice" is exactly what an operator reading
// this back wants to see. The timestamp is stamped here, in the owning writer,
// rather than travelling in from the tool — an observation belongs to whoever
// records it.
//
// The ordinal is returned because the caller needs a stable, distinct name for
// the user-facing notice it publishes next. Taking it from the durable record
// rather than from an in-process counter is what keeps two bypasses either side
// of a runner restart from landing on the same wire identifier.
func (s *StatusPatcher) RecordCompletionBypass(ctx context.Context, b completion.Bypass) (int, error) {
	rec := spiceboxv1alpha1.CompletionBypass{
		Time:   metav1.Now(),
		Reason: b.Reason,
	}
	for _, u := range b.Unmet {
		rec.Requirements = append(rec.Requirements, u.Key)
		rec.Details = append(rec.Details, u.Missing)
	}
	if s.local {
		// Capture for in-process callers; no CR to patch.
		s.localBypasses = append(s.localBypasses, rec)
		return len(s.localBypasses), nil
	}
	ordinal := 0
	err := s.mutate(ctx, func(sess *spiceboxv1alpha1.AgentSession) {
		sess.Status.CompletionBypasses = append(sess.Status.CompletionBypasses, rec)
		// Assigned inside the mutation so a conflict retry — which re-reads and
		// re-applies — reports the ordinal of the write that actually landed.
		ordinal = len(sess.Status.CompletionBypasses)
	})
	if err != nil {
		return 0, err
	}
	return ordinal, nil
}

// LocalCompletionBypasses returns the bypasses captured by
// RecordCompletionBypass on a local (in-process) patcher, in order. Nil for a
// CR-backed patcher, whose record lives on the AgentSession itself.
func (s *StatusPatcher) LocalCompletionBypasses() []spiceboxv1alpha1.CompletionBypass {
	return s.localBypasses
}

// WriteFailed writes the terminal failed state with the given reason.
func (s *StatusPatcher) WriteFailed(ctx context.Context, reason, message string) error {
	if s.local {
		// Idempotent: don't overwrite a prior failure capture.
		if s.localFailure == nil {
			s.localFailure = &LocalFailure{Reason: reason, Message: message}
		}
		return nil
	}
	return s.mutate(ctx, func(sess *spiceboxv1alpha1.AgentSession) {
		// Idempotent: don't overwrite a prior terminal state.
		if isTerminal(sess.Status.Phase) {
			return
		}
		sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseFailed
		sess.Status.FailureReason = reason
		now := metav1.Now()
		sess.Status.FinishedAt = &now
		sess.Status.AwaitingUserInputSince = nil
		meta.SetStatusCondition(&sess.Status.Conditions, metav1.Condition{
			Type: spiceboxv1alpha1.AgentSessionConditionFailed, Status: metav1.ConditionTrue,
			Reason: reason, Message: message, ObservedGeneration: sess.Generation,
		})
	})
}

// WriteAwaitingRetry writes the AwaitingRetry phase + condition for a
// channel-attached session that hit a recoverable stopping point (a
// Provider.Send error, or a provider content-policy refusal). The pod exits
// clean (same lifecycle as Idle exit) and channelsd's session_watcher posts the
// Retry button; RetryAttempts is incremented so the watcher can dedup
// per-attempt. reason (e.g. ReasonAgentSessionProviderErr vs.
// ReasonAgentSessionRefusal) lands on status.FailureReason and the condition's
// Reason so the watcher and `kubectl describe` can tell the causes apart.
//
// No-op in local mode — kubectl-driven sessions keep the terminal Failed UX
// (exit code + LocalFailure capture).
func (s *StatusPatcher) WriteAwaitingRetry(ctx context.Context, reason, message string) error {
	if s.local {
		// Local mode never enters AwaitingRetry — callers must route to
		// WriteFailed instead. Captured as LocalFailure for the in-process
		// caller's read-back path; mirrors WriteFailed's local branch.
		if s.localFailure == nil {
			s.localFailure = &LocalFailure{
				Reason:  reason,
				Message: message,
			}
		}
		return nil
	}
	return s.mutate(ctx, func(sess *spiceboxv1alpha1.AgentSession) {
		// Idempotent: don't transition out of a terminal state.
		if isTerminal(sess.Status.Phase) {
			return
		}
		sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry
		sess.Status.FailureReason = reason
		sess.Status.RetryAttempts++
		// Clear the Idle condition if set — we are no longer idle.
		meta.RemoveStatusCondition(&sess.Status.Conditions, spiceboxv1alpha1.AgentSessionConditionIdle)
		meta.SetStatusCondition(&sess.Status.Conditions, metav1.Condition{
			Type:               spiceboxv1alpha1.AgentSessionConditionAwaitingRetry,
			Status:             metav1.ConditionTrue,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: sess.Generation,
		})
	})
}

// WriteIdle patches the AgentSession to phase=Idle. Used by the channel-
// attached loop in two cases:
//   - a parking tool (await_user_message, or a delegated child's ask_parent)
//     returns IdleExit (TTL expired or context cancelled): pass
//     ReasonAgentSessionAwaitingUserMsg.
//   - agent_work_complete was called: pass ReasonAgentSessionAgentWorkComplete.
//
// The pod exits cleanly; the operator's wake-annotation observation is what
// spawns the next runner.
func (s *StatusPatcher) WriteIdle(ctx context.Context, reason string) error {
	s.localIdle = true
	now := metav1.Now()
	msg := "session idle"
	switch reason {
	case spiceboxv1alpha1.ReasonAgentSessionAwaitingUserMsg:
		msg = "agent yielded; idle TTL expired"
	case spiceboxv1alpha1.ReasonAgentSessionAgentWorkComplete:
		msg = "agent_work_complete called; session idle"
	}
	return s.mutate(ctx, func(sess *spiceboxv1alpha1.AgentSession) {
		sess.Status.Phase = spiceboxv1alpha1.AgentSessionPhaseIdle
		sess.Status.AwaitingUserInputSince = nil
		meta.SetStatusCondition(&sess.Status.Conditions, metav1.Condition{
			Type:               spiceboxv1alpha1.AgentSessionConditionIdle,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: now,
			Reason:             reason,
			Message:            msg,
		})
	})
}

// WakeRequestOutcome is what one RequestWake attempt did. The caller needs the
// distinction: "already pending" means the wake is someone else's problem and
// the loop is done, while "not parked" is inconclusive and worth another look.
type WakeRequestOutcome string

const (
	// WakeStamped: this call wrote the annotation; the operator will respawn.
	WakeStamped WakeRequestOutcome = "stamped"
	// WakeAlreadyPending: an unconsumed request is already on the object
	// (channelsd got there first for the same message).
	WakeAlreadyPending WakeRequestOutcome = "already-pending"
	// WakeNotParked: the session is not (or not yet) parked with its pod gone,
	// so a wake annotation would not respawn anything.
	WakeNotParked WakeRequestOutcome = "not-parked"
)

// WakeRequest is one RequestWake attempt's result, including the phase it
// decided from so the caller can log why nothing was stamped.
type WakeRequest struct {
	Outcome WakeRequestOutcome
	Phase   string
}

// RequestWake stamps the wake-requested-at annotation on the runner's OWN
// AgentSession, asking the operator to respawn a runner for work this one can no
// longer do. Sole caller: the exiting runner's post-Idle inbox re-check (see
// Loop.idleWithWakeRecheck). A channel message appended after this runner's last
// drain but before phase=Idle was visible to channelsd is invisible to both
// sides — channelsd saw a stale Running snapshot and left delivery to a NATS
// wakeup this pod will never receive, and the runner had stopped looking.
//
// A metadata patch, not a status patch: the operator's shouldWake reads an
// annotation, and reusing that one channel keeps channelsd's wake and the
// runner's indistinguishable to it. Re-reads and retries on optimistic-lock
// conflict, because the operator writes this object's status concurrently.
//
// Both skips are evaluated against the FRESH read so the optimistic lock makes
// check-then-write atomic:
//
//   - Not WakeEligible: nothing would respawn from the annotation right now.
//     Inconclusive, not final — see idleWithWakeRecheck.
//   - Already WakePending: channelsd got there first for this same message. A
//     second, later timestamp is actively harmful — if the operator consumes the
//     first request before our patch lands, our newer annotation outlives
//     status.lastWakeAt and fires a SECOND respawn once that runner idles, whose
//     drain finds nothing and produces a spurious turn.
func (s *StatusPatcher) RequestWake(ctx context.Context) (WakeRequest, error) {
	if s.local {
		return WakeRequest{Outcome: WakeNotParked}, nil
	}
	var out WakeRequest
	// Same retry policy as a status patch, and for the same reason: the
	// agentsessionidentity webhook covers `agentsessions` as well as
	// `agentsessions/status`, so an operator restart refuses this annotation
	// too — and a refused wake strands the message that landed as this runner
	// idled until the user speaks again.
	if err := s.patchWithRetry(ctx, "wake annotation patch", func() error {
		out = WakeRequest{}
		var sess spiceboxv1alpha1.AgentSession
		// s.reader, not s.c: this must observe the WriteIdle we just made.
		if err := s.reader.Get(ctx, s.key, &sess); err != nil {
			return err
		}
		phase := sess.Status.Phase
		if !spiceboxv1alpha1.WakeEligible(&sess) {
			out = WakeRequest{Outcome: WakeNotParked, Phase: phase}
			return nil
		}
		if spiceboxv1alpha1.WakePending(&sess) {
			out = WakeRequest{Outcome: WakeAlreadyPending, Phase: phase}
			return nil
		}
		base := sess.DeepCopy()
		if sess.Annotations == nil {
			sess.Annotations = map[string]string{}
		}
		sess.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt] = time.Now().UTC().Format(time.RFC3339Nano)
		if err := s.c.Patch(ctx, &sess, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		out = WakeRequest{Outcome: WakeStamped, Phase: phase}
		return nil
	}); err != nil {
		return WakeRequest{}, err
	}
	return out, nil
}

// RecordObservedPin upserts (by Name) one per-session pin observation.
func (s *StatusPatcher) RecordObservedPin(ctx context.Context, name string, pin spiceboxv1alpha1.PinRecord) error {
	return s.mutate(ctx, func(sess *spiceboxv1alpha1.AgentSession) {
		sess.Status.ObservedPins = spiceboxv1alpha1.UpsertObservedPin(sess.Status.ObservedPins, name, pin)
	})
}

// RecordSidecarReachability upserts (by name — the LLM-facing prefix) the
// runner's per-session live-probe result for one sidecar onto
// status.sidecarReachability. Both the boot pass and the mid-session refresher
// call it, so a sidecar that is up but unreachable or drifted shows in status
// instead of being silent. reachable=true clears Unreachable; reachable=false
// records the reason. Runner-owned field (the operator never writes it),
// optimistic-lock retried; no-op in local mode.
func (s *StatusPatcher) RecordSidecarReachability(ctx context.Context, name string, reachable bool, observed []string, unreachable string) error {
	return s.mutate(ctx, func(sess *spiceboxv1alpha1.AgentSession) {
		now := metav1.Now()
		sess.Status.SidecarReachability = spiceboxv1alpha1.UpsertSidecarReachability(
			sess.Status.SidecarReachability,
			spiceboxv1alpha1.SidecarReachability{
				Name:          name,
				Reachable:     reachable,
				ObservedTools: observed,
				Unreachable:   unreachable,
				ObservedAt:    &now,
			})
	})
}

// RecordCredentialAuthFailure upserts (by origin) one per-session
// credential-update corroboration observation: the platform's OWN evidence that
// a tool call at this origin failed the way this provider's credentials fail.
// count is the consecutive auth-shaped failure count at the transition — see
// pkg/agent/tool/authfail for why this is called once per transition rather than
// once per failing call. Runner-owned field (the operator only reads it),
// optimistic-lock retried; no-op in local mode. With
// ClearCredentialAuthFailure, satisfies authfail.StatusWriter.
func (s *StatusPatcher) RecordCredentialAuthFailure(ctx context.Context, origin string, count int32) error {
	return s.mutate(ctx, func(sess *spiceboxv1alpha1.AgentSession) {
		now := metav1.Now()
		sess.Status.CredentialAuthFailures = spiceboxv1alpha1.UpsertCredentialAuthFailure(
			sess.Status.CredentialAuthFailures,
			spiceboxv1alpha1.CredentialAuthFailure{Origin: origin, Count: count, ObservedAt: &now},
		)
	})
}

// ClearCredentialAuthFailure removes origin's corroboration observation after
// a successful call there — a credential that just worked is not one that
// needs replacing. Removing the LAST entry leaves the (omitempty) field absent
// from the marshaled status, so the merge patch carries a field deletion and
// the observation genuinely disappears rather than lingering as an empty list.
func (s *StatusPatcher) ClearCredentialAuthFailure(ctx context.Context, origin string) error {
	return s.mutate(ctx, func(sess *spiceboxv1alpha1.AgentSession) {
		sess.Status.CredentialAuthFailures = spiceboxv1alpha1.RemoveCredentialAuthFailure(
			sess.Status.CredentialAuthFailures, origin)
	})
}

// AppendRunnerNote appends a timestamped note. Used for stale-runner-on-
// terminal-session and other operator/runner observability events. Capped
// oldest-first, like AppendActiveWidget: nothing prunes runnerNotes, the CRD
// declares no maxItems, and mutate's merge patch re-sends the whole list on
// every append, so an unbounded list costs a growing write per note and a
// growing object for every reader of the AgentSession.
func (s *StatusPatcher) AppendRunnerNote(ctx context.Context, msg string) error {
	return s.mutate(ctx, func(sess *spiceboxv1alpha1.AgentSession) {
		notes := append(sess.Status.RunnerNotes, spiceboxv1alpha1.RunnerNote{
			Time: metav1.Now(), Message: msg,
		})
		if len(notes) > maxRunnerNotes {
			notes = notes[len(notes)-maxRunnerNotes:]
		}
		sess.Status.RunnerNotes = notes
	})
}

// WriteSatisfiedSecretOutput records that a secret-output handle has been
// published to the per-session Secret. Idempotent: a second call with the
// same handle is a no-op (the original entry is preserved). The value is
// never stored in status; this is observability only. name is the
// toolspec's secretOutput.name — recorded so the sandbox tool's fast-fail
// pre-check can enforce write-once per name without reading the Secret.
func (s *StatusPatcher) WriteSatisfiedSecretOutput(ctx context.Context, name, handle, secretName string) error {
	return s.mutate(ctx, func(sess *spiceboxv1alpha1.AgentSession) {
		for _, existing := range sess.Status.SatisfiedSecretOutputs {
			if existing.Handle == handle {
				return // idempotent: already recorded
			}
		}
		now := metav1.Now()
		sess.Status.SatisfiedSecretOutputs = append(sess.Status.SatisfiedSecretOutputs,
			spiceboxv1alpha1.SecretOutputCompletion{
				Name:       name,
				Handle:     handle,
				SecretName: secretName,
				WrittenAt:  &now,
			})
	})
}

// setAwaitingUserInput patches ONLY status.awaitingUserInputSince via a
// resourceVersion-free JSON merge patch (mirrors agentstatus.WriteOwned's
// non-condition path): an unincluded field is never re-sent, so it never
// conflicts with concurrent operator/channelsd writes. ts==nil clears it
// (merge-patch null).
func (s *StatusPatcher) setAwaitingUserInput(ctx context.Context, ts *metav1.Time) error {
	if s.local {
		return nil
	}
	var body string
	if ts == nil {
		body = `{"status":{"awaitingUserInputSince":null}}`
	} else {
		b, _ := ts.MarshalJSON()
		body = fmt.Sprintf(`{"status":{"awaitingUserInputSince":%s}}`, string(b))
	}
	return s.c.Status().Patch(ctx,
		&spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: s.key.Name, Namespace: s.key.Namespace}},
		client.RawPatch(types.MergePatchType, []byte(body)))
}

// SetAwaitingUserInput records the current time as awaitingUserInputSince via
// an RV-free merge patch. Called by OnAwaitYield (best-effort).
func (s *StatusPatcher) SetAwaitingUserInput(ctx context.Context) error {
	now := metav1.Now()
	return s.setAwaitingUserInput(ctx, &now)
}

// ClearAwaitingUserInput removes awaitingUserInputSince via an RV-free merge
// patch (null field). Called by OnAwaitResume and terminal writes (best-effort).
func (s *StatusPatcher) ClearAwaitingUserInput(ctx context.Context) error {
	return s.setAwaitingUserInput(ctx, nil)
}

// setPinnedMessage patches ONLY status.pinnedMessage via an RV-free JSON
// merge patch (mirrors setAwaitingUserInput above): an unincluded field is
// never re-sent, so it never conflicts with a concurrent operator/channelsd
// write. p==nil clears the projection (merge-patch null) — the session's
// input kind carries no opening message to project onto.
func (s *StatusPatcher) setPinnedMessage(ctx context.Context, p *spiceboxv1alpha1.PinnedMessageStatus) error {
	if s.local {
		return nil
	}
	var body string
	if p == nil {
		body = `{"status":{"pinnedMessage":null}}`
	} else {
		b, err := json.Marshal(p)
		if err != nil {
			return fmt.Errorf("marshal pinnedMessage patch: %w", err)
		}
		body = fmt.Sprintf(`{"status":{"pinnedMessage":%s}}`, string(b))
	}
	return s.c.Status().Patch(ctx,
		&spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: s.key.Name, Namespace: s.key.Namespace}},
		client.RawPatch(types.MergePatchType, []byte(body)))
}

// AskParent records one question from a delegated child to the agent that
// delegated to it, on status.parentExchange, and returns the exchange number
// it was recorded as. The SubagentRequest controller mirrors it onto the
// request the child answers, which is what lets the parent's delegate call
// return with the question instead of blocking to a terminal phase.
//
// Read-modify-write through mutate rather than the RV-free merge patch
// setAwaitingUserInput uses, because the exchange number is derived from the
// value already stored: it is the ONE thing a parent has to tell a new
// question from the one it just answered, so it must never restart at 1 after
// the child's pod is reaped mid-wait and re-hydrated. Reading it back from the
// CR is what makes it survive that.
//
// Returns 0 and an error if the write failed; the caller must surface that to
// the child's model rather than parking on a question nobody will ever see.
// In local (kubectl-driven) mode there is no CR to record on and no parent to
// read it, so this returns an error rather than silently reporting success for
// a question that went nowhere.
func (s *StatusPatcher) AskParent(ctx context.Context, question string) (int64, error) {
	if s.local {
		return 0, stderrors.New("ask_parent: this session has no AgentSession record to ask through")
	}
	var recorded int64
	err := s.mutate(ctx, func(sess *spiceboxv1alpha1.AgentSession) {
		next := int64(1)
		if pe := sess.Status.ParentExchange; pe != nil {
			next = pe.Exchange + 1
		}
		recorded = next
		sess.Status.ParentExchange = &spiceboxv1alpha1.ParentExchange{
			Exchange: next,
			Pending:  true,
			Question: question,
		}
	})
	if err != nil {
		return 0, err
	}
	return recorded, nil
}

// RequestInput records one mid-flight ask for DATA from a delegated child and
// PARKS it, the same way a question does.
//
// # Why it parks
//
// Because the only party who can answer is blocked until it does. `delegate`
// polls its child and returns control to the parent on four phases only —
// Succeeded, AwaitingParent, Denied, Failed — so while a child merely runs,
// its parent's model never gets a turn and can never call send_input. A
// non-blocking data request is unanswerable by construction: the request would
// sit until the delegation ended, having reached nobody who could act on it.
//
// So the ask rides ParentExchange, which is exactly the mechanism that hands
// control back. That reuses the whole path — the phase, the exchange budget
// and its anti-forgery reasoning, delegate's return arm, reply_to_subagent's
// resume — rather than adding a second one that would have to earn all of it
// again.
//
// # Two records, one counter
//
// InputRequest carries the STRUCTURED ask (which slot, and why) for send_input
// and the disclosure card to read; ParentExchange carries the control flow and
// the sentence the parent's model is shown. Both take the ParentExchange
// counter so the two can never disagree about which ask is outstanding.
//
// Read-modify-write for the same reason AskParent is: that number must not
// restart at 1 after the child's pod is reaped mid-wait, or the parent cannot
// tell a new ask from the one it just answered.
//
// Returns an error in local (kubectl-driven) mode rather than reporting
// success for a request that went nowhere.
func (s *StatusPatcher) RequestInput(ctx context.Context, slot, why string) (int64, error) {
	if s.local {
		return 0, stderrors.New("request_input: this session has no AgentSession record to ask through")
	}
	var recorded int64
	err := s.mutate(ctx, func(sess *spiceboxv1alpha1.AgentSession) {
		next := int64(1)
		if pe := sess.Status.ParentExchange; pe != nil {
			next = pe.Exchange + 1
		}
		recorded = next
		sess.Status.InputRequest = &spiceboxv1alpha1.InputRequest{
			Slot:     slot,
			Why:      why,
			Exchange: next,
			Pending:  true,
		}
		// PLATFORM-COMPOSED, not the child's own sentence. The parent is being
		// asked to perform a specific act — fill a named slot — and the words
		// that say so must be the platform's, with the child's reason quoted
		// inside as the untrusted part it is.
		sess.Status.ParentExchange = &spiceboxv1alpha1.ParentExchange{
			Exchange: next,
			Pending:  true,
			Question: fmt.Sprintf(
				"It needs data for its %q input slot before it can continue. "+
					"Send it with send_input(slot=%q), naming the tool call whose result holds it — "+
					"do not paste the data into a reply. Its reason follows.\n\n%s",
				slot, slot, why),
		}
	})
	if err != nil {
		return 0, err
	}
	return recorded, nil
}

// ClearParentPending marks the outstanding question answered, leaving the
// exchange number and the question text in place: the number must stay
// monotonic for AskParent to increment from, and the text is the record of
// what was last asked.
//
// The read comes FIRST and returns early when nothing is outstanding, so the
// overwhelming majority of sessions — every one that never asks anything —
// pay one Get here and no write at all.
//
// Called from the two places an ANSWER is known to have arrived, and only
// those: the loop's resume hook (OnAwaitResume, which fires when InboundCh
// delivers into a live park) and Run's resume drain, when that drain actually
// placed an inbound turn. Deliberately NOT from the idle path, and
// deliberately NOT unconditionally at Run start — a child that parked, timed
// out to Idle and had its pod reaped is still waiting for its parent, and so
// is one whose pod was killed mid-park and restarted. The question is cleared
// by the reply, never by the runner being alive again.
func (s *StatusPatcher) ClearParentPending(ctx context.Context) error {
	if s.local {
		return nil
	}
	var sess spiceboxv1alpha1.AgentSession
	if err := s.reader.Get(ctx, s.key, &sess); err != nil {
		return err
	}
	if pe := sess.Status.ParentExchange; pe == nil || !pe.Pending {
		return nil
	}
	return s.mutate(ctx, func(sess *spiceboxv1alpha1.AgentSession) {
		if pe := sess.Status.ParentExchange; pe != nil {
			pe.Pending = false
		}
		// A data request rides the same park, so the same resume clears it.
		// Leaving it pending would keep the request mirrored as outstanding on
		// the parent's object forever, and the parent would go on being shown
		// an ask it already answered.
		if ir := sess.Status.InputRequest; ir != nil {
			ir.Pending = false
		}
	})
}

// AppendActiveWidget appends ref onto status.activeWidgets, capped to the most
// recent maxActiveWidgets entries (oldest dropped first), via a
// resourceVersion-free JSON merge patch scoped to that ONE field — like
// setAwaitingUserInput, an unincluded field is never re-sent, so this write
// never conflicts with a concurrent operator/channelsd write to another field.
//
// Computing the capped list needs the current value, so this does one plain Get
// before the patch, NOT wrapped in mutate's optimistic-lock retry loop. Safe
// because status.activeWidgets is runner-owned and applyUIResource calls this
// only from the single per-tool-result loop goroutine (the single-writer
// invariant documented on Loop.uiEscalated), so nothing races the read against
// the patch.
func (s *StatusPatcher) AppendActiveWidget(ctx context.Context, ref spiceboxv1alpha1.WidgetRef) error {
	if s.local {
		return nil
	}
	var sess spiceboxv1alpha1.AgentSession
	if err := s.c.Get(ctx, s.key, &sess); err != nil {
		return err
	}
	widgets := append(sess.Status.ActiveWidgets, ref)
	if len(widgets) > maxActiveWidgets {
		widgets = widgets[len(widgets)-maxActiveWidgets:]
	}
	body, err := json.Marshal(widgets)
	if err != nil {
		return fmt.Errorf("marshal activeWidgets: %w", err)
	}
	patch := fmt.Sprintf(`{"status":{"activeWidgets":%s}}`, string(body))
	return s.c.Status().Patch(ctx,
		&spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: s.key.Name, Namespace: s.key.Namespace}},
		client.RawPatch(types.MergePatchType, []byte(patch)))
}

func isTerminal(phase string) bool {
	return phase == spiceboxv1alpha1.AgentSessionPhaseSucceeded ||
		phase == spiceboxv1alpha1.AgentSessionPhaseFailed
}

// Get returns the current AgentSession (used in tests).
func (s *StatusPatcher) Get(ctx context.Context, into *spiceboxv1alpha1.AgentSession) error {
	if s.local {
		return nil
	}
	return s.c.Get(ctx, s.key, into)
}

// LocalFailure returns the (reason, message) pair captured by WriteFailed
// when the patcher was created via LocalStatusPatcher. nil before any
// failure has been recorded.
func (s *StatusPatcher) LocalFailure() *LocalFailure { return s.localFailure }

// LocalIdle reports whether WriteIdle was called. See localIdle's own comment
// for why an in-process caller needs this to exist at all.
func (s *StatusPatcher) LocalIdle() bool { return s.localIdle }

// IsLocal reports whether this patcher was constructed in local mode
// (no AgentSession CR to patch). Used by the runner's failure
// classifier to decide between WriteAwaitingRetry (channel-attached)
// and WriteFailed (kubectl-driven).
func (s *StatusPatcher) IsLocal() bool { return s.local }

// LocalResult returns the AgentResult captured by WriteSucceeded when the
// patcher is in local mode. Returns nil if no terminal-success write has
// happened (or the patcher is non-local). Used by the gen-agent driver to
// surface the agent's final summary back to its caller.
func (s *StatusPatcher) LocalResult() *spiceboxv1alpha1.AgentResult {
	return s.localResult
}

func (s *StatusPatcher) mutate(ctx context.Context, fn func(*spiceboxv1alpha1.AgentSession)) error {
	if s.local {
		return nil
	}
	return s.patchWithRetry(ctx, "status patch", func() error {
		var sess spiceboxv1alpha1.AgentSession
		if err := s.c.Get(ctx, s.key, &sess); err != nil {
			return err
		}
		patch := client.MergeFromWithOptions(sess.DeepCopy(), client.MergeFromWithOptimisticLock{})
		fn(&sess)
		return s.c.Status().Patch(ctx, &sess, patch)
	})
}

// patchWithRetry runs one read-modify-write attempt until it succeeds or fails
// for a reason the runner cannot wait out. Two failure classes, two policies:
//
//   - CONFLICT — the optimistic lock was lost to the operator's or channelsd's
//     concurrent write of a different field. Somebody else's write already
//     landed, so re-read and re-apply IMMEDIATELY; maxRetries attempts, no
//     sleeping. This is the contended-but-healthy path and must stay cheap.
//
//   - SERVER OUTAGE — the API server could not complete the request at all,
//     typically a failurePolicy: Fail admission webhook whose Service has no
//     endpoints, which is what the agentsessionidentity gate looks like while
//     the operator serving it is replaced. The write is VALID and will be
//     admitted once the webhook is back, so wait it out with capped, jittered
//     exponential backoff inside s.outage.budget.
//
// Everything else — a denial (403), NotFound, an invalid patch — is a verdict
// about this write, not a transient condition, and is returned at once. Both
// boundaries of the wait are logged at INFO (no-silent-errors) so a paused
// session and its recovery are visible in runner logs rather than inferred.
func (s *StatusPatcher) patchWithRetry(ctx context.Context, what string, attempt func() error) error {
	session := s.key.Namespace + "/" + s.key.Name
	conflicts := 0
	var waited time.Duration
	delay := s.outage.initial
	for {
		err := attempt()
		switch {
		case err == nil:
			if waited > 0 {
				slog.Default().Info("API write admitted after waiting out a server outage",
					"session", session, "write", what, "waited", waited.String())
			}
			return nil

		case errors.IsConflict(err):
			conflicts++
			if conflicts >= maxRetries {
				return errors.NewConflict(spiceboxv1alpha1.Resource("agentsessions"), s.key.Name, nil)
			}

		case isServerOutage(err):
			// This check is also what terminates the loop: it guarantees
			// waited < budget below, so the clamp yields a strictly positive
			// d and waited grows by at least that much every pass. Weaken it
			// and the clamp goes non-positive and the loop spins.
			if waited >= s.outage.budget {
				slog.Default().Info("giving up on an API write after waiting out the server-outage budget; the runner will exit and be restarted",
					"session", session, "write", what, "waited", waited.String(), "err", err.Error())
				return err
			}
			d := min(wait.Jitter(delay, 0.2), s.outage.budget-waited)
			if waited == 0 {
				slog.Default().Info("API write refused because the server could not complete it (admission webhook unreachable during an operator restart?); waiting it out rather than failing the session",
					"session", session, "write", what, "budget", s.outage.budget.String(), "err", err.Error())
			}
			timer := time.NewTimer(d)
			select {
			case <-ctx.Done():
				timer.Stop()
				slog.Default().Info("context cancelled while waiting out a server outage; surfacing the original write error",
					"session", session, "write", what, "err", err.Error())
				return err
			case <-timer.C:
			}
			waited += d
			if delay < s.outage.cap {
				delay = min(delay*2, s.outage.cap)
			}

		default:
			return err
		}
	}
}

// isServerOutage reports whether err is the API server saying it could not
// COMPLETE the request for a server-side reason the caller can outlive.
//
// Classified by HTTP status, not by k8s reason string: an undialable
// failurePolicy: Fail webhook comes back as 500 ("Internal error occurred:
// failed calling webhook …"), and that status's reason field is not dependable
// across API-server versions.
//
// A webhook DENIAL (403 Forbidden) is deliberately excluded: it is a verdict, so
// retrying would spin for the whole budget on a write that will never be
// admitted and blunt the identity-pinning gate into a slow failure.
func isServerOutage(err error) bool {
	var status errors.APIStatus
	if !stderrors.As(err, &status) {
		return false
	}
	switch status.Status().Code {
	case http.StatusInternalServerError,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
		http.StatusRequestTimeout,
		http.StatusTooManyRequests:
		return true
	}
	return false
}
