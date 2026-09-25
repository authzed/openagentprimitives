package runner

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/provenance"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/toolcatalog"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/authzed/openagentprimitives/pkg/x/besteffort"

	"k8s.io/apimachinery/pkg/types"
)

// progressf is a nil-safe shorthand for l.Progress.
func (l *Loop) progressf(format string, args ...any) {
	if l.Progress != nil {
		l.Progress(format, args...)
	}
}

// streamSink is the single OnEvent passed to the provider per call. It
// feeds usage events to the progress reporter (for the live token/time
// indicator) and forwards every event to OnStreamEvent (channelsd's
// stream-delta path). Either consumer may be absent.
func (l *Loop) streamSink(e llm.StreamEvent) {
	if e.Type == llm.StreamEventUsage && e.Usage != nil {
		l.progress.observeUsage(e.Usage.InputTokens, e.Usage.OutputTokens)
	}
	if l.OnStreamEvent != nil {
		l.OnStreamEvent(e)
	}
}

// Run drives the loop until termination. Always writes a terminal status
// (Succeeded or Failed) before returning. Returns nil on normal terminal
// exit; non-nil only if the *status write itself* failed.
func (l *Loop) Run(ctx context.Context) error {
	l.emitLifecycleSignal(ctx, lifecycle.SigSessionStarted, nil)

	// Replay prior memory (for resume); on cold start this is empty.
	prior, err := l.Memory.ReadAll(ctx)
	if err != nil {
		return l.fail(ctx, spiceboxv1alpha1.ReasonAgentSessionMemoryDown, fmt.Sprintf("read memory: %v", err))
	}
	messages, nextIndex, hadInitialPrompt := replay(prior, l.SessionKey.Namespace+"/"+l.SessionKey.Name)
	// Seed the emit-time memory index from the durable replay base so a resumed
	// Run's first loop-side activity signal (start-of-turn active) sorts after
	// prior turns rather than from 0. Advanced per turn below.
	l.setLastAssistantTurnIndex(nextIndex)
	// Seed currentUserTurnIndex from whatever the transcript already shows, so
	// a resumed runner reports the session's actual last user turn instead of
	// "unknown" (-1) until the next inbound drains. drainInbox advances it
	// further for every turn drained after this point (see
	// setCurrentUserTurnIndex's doc).
	l.setCurrentUserTurnIndex(highestUserTurnIndex(prior))

	// A fresh/resumed Run has not yielded yet; clear any awaitingUserInputSince
	// left by a prior pod that crashed mid-await, so the silence watchdog isn't
	// wrongly suppressed for this run's work. The next await_user_message re-sets it.
	if l.Status != nil {
		if err := l.Status.ClearAwaitingUserInput(ctx); err != nil {
			slog.Default().Info("Run: clear stale awaitingUserInputSince failed (best-effort)",
				"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "err", err.Error())
		}
		// status.parentExchange.pending is deliberately NOT cleared alongside
		// it. The sibling field above is about THIS run's watchdog and is stale
		// by definition at Run start; a delegated child's pending question is
		// not — it stands until the parent's answer actually arrives, and a Run
		// starting is not evidence that it did. A pod killed while parked in
		// ask_parent (OOM, drain, eviction — status.runnerRestarts exists
		// because those happen) comes back with the question still unanswered,
		// and a wake can carry no message at all (see resumedWithNothingToDo
		// below). Clearing unconditionally here told the SubagentRequest
		// controller the child had resumed, which moved the request
		// AwaitingParent → Running and dropped AwaitingParentSince — so the
		// parent never saw the question, polled its full ceiling, and the
		// per-exchange bound that would have reclaimed the child was gone. For
		// a `task` child the re-ask is then refused on budget and the question
		// is lost outright. The clear now happens where the answer is: the
		// run-start drain below, and OnAwaitResume for a live park.

		// Publish what this session's tools can actually reach, so the CLI and
		// admin panel can answer "what can this agent do?" from the one
		// producer that holds the real tool envelope. Best-effort: a display
		// value must not cost a working session.
		if err := l.Status.PublishPermissionSurface(ctx, l.PlanGateSurface); err != nil {
			slog.Default().Info("Run: publish permissionSurface failed (best-effort; "+
				"capabilities views will be empty for this session)",
				"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
				"handles", len(l.PlanGateSurface), "err", err.Error())
		}
	}

	// Restart recovery + authority handoff ①: fold the signed transition log
	// to re-arm any pending decisions, then claim the live region. With no
	// LifecycleMemory wired (tests / kubectl dev runs) this is a no-op append.
	l.claimAndRecover(ctx)

	// Compute the set of tool_use IDs already delivered in prior turns. Used
	// only when ChannelAttached to skip re-dispatching tool_uses on resume.
	delivered := ComputeDeliveredToolUseIDs(prior)

	// Rebuild session-state stores from prior wrapped system_notes.
	if l.SessionContext != nil {
		if reg, ok := l.SessionContext.State.(*state.Registry); ok {
			ReplayStateNotes(prior, reg)
		}
	}

	// On cold start (no turn 0 yet), place the initial user prompt as turn 0.
	// For a scope-managed cold-start-eligible session, the runner drives the
	// cold-start flow first (publish metaagent_request → wait for cold_start_task)
	// and places the cleaned/raw/no turn-0 per the approver's decision; otherwise
	// the prompt is placed verbatim.
	if hadInitialPrompt {
		// A resume (turn-0 already in memory): cold-start never re-runs. Logged
		// so "cold start didn't fire" on a resume is explicable from logs.
		slog.Default().Info("cold-start: skipped (resume — initial prompt already in memory)",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name)
	}
	if !hadInitialPrompt {
		place := true
		content := []memory.ContentBlock{{Type: "text", Text: l.UserPrompt}}
		// A delegated child's data slots go in FRONT of the task text. The
		// parent handed these over by reference and the platform resolves them
		// here — the model never asks for them, which is what keeps
		// pt_tag_content's read door meaningful.
		//
		// A resolution failure does NOT fall through to an unprefixed prompt.
		// The child would then run believing its parent supplied nothing,
		// which is a different task from the one that was delegated, and it
		// would look like a parent that chose to withhold rather than a
		// platform that failed.
		if l.ResolveBoundSlots != nil {
			slots, serr := l.ResolveBoundSlots(ctx)
			if serr != nil {
				slog.Default().Info("data slots: could not resolve this session's bound inputs; refusing to start on a partial handoff",
					"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "err", serr.Error())
				return fmt.Errorf("resolving bound data slots: %w", serr)
			}
			if len(slots) > 0 {
				slog.Default().Info("data slots: placing bound inputs in the opening context",
					"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "slots", len(slots))
				content = append(SlotContentBlocks(slots), content...)
			}
		}
		eligible, ineligibleReason := l.coldStartEligibility()
		if eligible {
			slog.Default().Info("cold-start: eligible; driving scope review for initial prompt",
				"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name)
		} else {
			slog.Default().Info("cold-start: not eligible; placing initial prompt verbatim",
				"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "reason", ineligibleReason)
		}
		// Run the SessionStart executor when there is scope review to drive
		// (eligible) OR an identity choice to make (IdentityGatePending, set only
		// for a first-boot ask|dynamic session). The two are independent: an
		// ask|dynamic session with scope disabled still MUST gate its identity, or
		// it silently runs as the provisional agent. Static modes leave
		// IdentityGatePending false.
		if eligible || l.IdentityGatePending {
			// SessionStart point: the ColdStartScope hook drives scope review
			// (publish metaagent_request → wait cold_start_task → map); when
			// IdentityGatePending, the IdentityChoiceGate runs first. Both run on a
			// per-call executor bound to a host that captures their side-channel
			// outputs (the turn-0 placement; the userPassthrough handoff flag) and
			// whose Halt is suppressed so Run stays the SOLE terminal writer for
			// the fail-closed path.
			host := newRunnerHost(l, hostSession{
				Namespace: l.SessionKey.Namespace,
				Name:      l.SessionKey.Name,
				Class:     l.AgentName,
			})
			host.suppressHaltWrite = true
			out, _ := l.coldStartSessionStartExecutor(host, eligible).Run(ctx, pipeline.SessionStart, pipeline.Input{
				Session: pipeline.SessionRef{
					Namespace: l.SessionKey.Namespace,
					Name:      l.SessionKey.Name,
					Class:     l.AgentName,
				},
				Requester: l.StartedByCanonical,
				Turn:      &pipeline.TurnInfo{Text: l.UserPrompt},
			}, host)

			if out.Verdict == pipeline.Halt {
				if host.takeIdentityHandoff() {
					// Passthrough chosen: the gate emitted the signed
					// IdentityChoiceResolved{userPassthrough} event and the fold
					// already moved the session to phase=Pending. Exit NON-terminally
					// — never l.fail — so the operator re-drives the passthrough
					// credential-link flow and re-spawns the runner; returning nil
					// before any terminal write leaves the phase as folded.
					l.IdentityHandoffExited = true
					slog.Default().Info("identity handoff to passthrough; exiting non-terminally for operator re-drive",
						"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name)
					return nil
				}
				// FAIL CLOSED: scope review or the identity choice could not
				// complete (cold-start failure, or the gate's cancel / timeout /
				// non-interactive fail-closed). Do NOT run the agent. The hook
				// already delivered the user-visible notice (Decision.Notices), and
				// host.Halt was suppressed, so this l.fail is the single terminal
				// write. identityHaltReason keeps an identity halt from being
				// mislabelled ScopeReviewFailed, which would diverge from the gate's
				// IdentityChoiceCancelled fold and defeat the operator's
				// IdentityChoiceTimeout backstop.
				return l.fail(ctx, identityHaltReason(out.Reason),
					fmt.Sprintf("session start halted: %s", out.Reason))
			}

			found, csPlace, csContent := host.takeColdStartPlacement()
			if found && !csPlace {
				// Denied or empty remainder: there is no turn-0 for the agent to
				// run, and the deny/notice was already surfaced by authzd. End the
				// session cleanly WITHOUT dispatching the agent loop. Channel-
				// attached sessions go Idle (the conversation stays alive for a
				// follow-up); kubectl-driven sessions complete successfully with an
				// empty result. We must NOT fall through to run the agent.
				//
				// A DELEGATED child does neither, because neither ending reaches
				// the party that is waiting. Idle leaves its SubagentRequest
				// Running — reconcileChild resolves only on a terminal child phase
				// — so the parent's delegate / reply_to_subagent call polls to its
				// own 30-minute ceiling and reports a timeout for what was really
				// an immediate refusal. Succeeded is worse: it would hand the
				// parent an empty result as though the child had done the work and
				// found nothing to say. There is also no person here to read
				// authzd's notice, and respond_to_user is withheld from such a
				// session, so failing IS the only way the refusal travels.
				if l.DelegatedChild {
					return l.fail(ctx, spiceboxv1alpha1.ReasonAgentSessionColdStartDenied,
						"cold-start scope review left no turn to run: the delegated task was refused (or cleaned to nothing) before the agent started")
				}
				if l.ChannelAttached {
					l.emitLifecycleSignal(ctx, lifecycle.SigSessionIdle, map[string]string{"reason": "idle"})
					l.emitLifecycleEvent(ctx, lifecyclecore.AgentWorkComplete{Kubectl: false})
					l.fireSessionEnd(ctx, "idle")
					return l.idleWithWakeRecheck(ctx, spiceboxv1alpha1.ReasonAgentSessionAgentWorkComplete)
				}
				l.emitLifecycleSignal(ctx, lifecycle.SigSessionCompleted, map[string]string{"outcome": "succeeded"})
				l.emitLifecycleEvent(ctx, lifecyclecore.AgentWorkComplete{Kubectl: true})
				l.fireSessionEnd(ctx, "completed")
				return l.Status.WriteSucceeded(ctx, spiceboxv1alpha1.AgentResult{})
			}
			if found {
				// Usable decision with a turn to place (cleaned or raw).
				place, content = csPlace, csContent
			}
			// !found ⇒ the hook never set placement (should not happen for an
			// eligible session on the Allow path); keep the verbatim prompt.
		}
		if place {
			t := memory.Turn{
				Index: 0, Role: "user",
				Content:   content,
				CreatedAt: time.Now().UTC(),
				Author:    l.authorForStarter(),
			}
			if err := l.Memory.Append(ctx, t); err != nil {
				return l.fail(ctx, spiceboxv1alpha1.ReasonAgentSessionMemoryDown, fmt.Sprintf("append initial prompt: %v", err))
			}
			if !t.Author.Empty() {
				// Turn 0 IS an inbound human message but never passes through
				// drainInbox, which sets this same field. Without it,
				// lastInboundAuthor stays empty for the session's very first human
				// turn and hydrateSpeakerProfile silently skips it. No-op for
				// kubectl/bento-driven sessions, where authorForStarter() is empty.
				l.lastInboundAuthor = t.Author
			}
			messages = append(messages, llm.Message{
				Role:    "user",
				Content: contentBlocksFromMemory(content),
			})
			nextIndex = 1
			l.CurrentInboxIdx = 0 // initial prompt has inbox-Index 0 (channelsd convention)
			if l.ChannelAttached && l.Notify != nil {
				l.Notify(ctx, l.agentDisplayName()+" thinking…")
			}
		}
	}

	// The message that created this session may still be arriving: its text is
	// turn 0 above, but any files it carried are written separately by channelsd,
	// after a per-file network round trip this runner was spawned in the middle
	// of. Wait for that write before draining, so the model sees the whole
	// message rather than the half that fit in spec.Prompt. No-op when the
	// message carried no files.
	//
	// Cold start only: once turn 0 is in memory it has already been dispatched,
	// so waiting can no longer put the files in front of the model that needs
	// them — a later drain picks them up either way. The gate also bounds the
	// pathological case: a session whose attachment write failed outright carries
	// the annotation forever, and an ungated fence would re-spend the whole
	// budget on every resume for a turn that is never coming.
	if !hadInitialPrompt {
		l.awaitInboundAttachmentTurn(ctx)
	}

	// Drain any "inbox"-role turns channelsd wrote while this session was
	// parked (e.g. a resumed session that idle-parked, then got a human
	// reply before the runner came back up). drainInbox places them as
	// real "user" turns at runner-assigned indices; the runner is the
	// sole writer of user/assistant turns.
	if placed, drained, derr := l.drainInbox(ctx, nextIndex); derr != nil {
		return l.fail(ctx, spiceboxv1alpha1.ReasonAgentSessionMemoryDown, fmt.Sprintf("drain inbox: %v", derr))
	} else {
		for _, t := range placed {
			messages = append(messages, llm.Message{Role: "user", Content: contentBlocksFromMemory(t.Content)})
			// Mirror an annotation batch (channelsd defers its raw echo) as a
			// human-readable KindUserEcho once it's durably placed. Never aborts
			// the drain on failure — see maybeEchoAnnotationTurn's doc.
			l.maybeEchoAnnotationTurn(ctx, t)
		}
		nextIndex = drained
		// A delegated child's outstanding question is answered by a MESSAGE, so
		// this is where a restarted child learns its answer arrived: the drain
		// placing at least one inbound turn is the evidence Run start does not
		// have. Nothing placed means the runner came back for some other reason
		// (a wake with no message, a crash, a UI-data-binding respawn) and the
		// question still stands — see the Run-start block above for what
		// clearing it anyway costs. The live-park counterpart is
		// OnAwaitResume, which fires only when InboundCh actually delivers.
		// Data bound since this child last looked. A child parked on
		// request_input is resumed by its parent's reply, and the DATUM
		// arrives on a separate path — the operator binds it after grading,
		// possibly after a person approved. So the resume is where the two
		// meet: without this the child wakes to "here is your answer" and
		// still cannot see the thing it asked for.
		// awaitBind on len(placed) > 0: a placed turn IS the parent's answer
		// arriving, which is the only thing that puts a binding in flight. A
		// resume that drained nothing has nothing to wait for.
		if blocks := l.deliverNewBoundSlots(ctx, prior, len(placed) > 0); len(blocks) > 0 {
			messages = append(messages, llm.Message{Role: "user", Content: contentBlocksFromMemory(blocks)})
		}
		if len(placed) > 0 && l.Status != nil {
			if err := l.Status.ClearParentPending(ctx); err != nil {
				slog.Default().Info("Run: clear parentExchange.pending after the resume drain failed (best-effort; "+
					"the request keeps reporting this child as awaiting its parent until a later resume clears it "+
					"or the per-exchange bound expires)",
					"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
					"placed", len(placed), "err", err.Error())
			}
		}
	}

	// Build the tool list for the LLM request. Rebuilt each turn from
	// l.Tools so that tools added mid-session by ToolRefresher (newly-ready
	// secret-gated sidecars) are exposed to the LLM on the next turn.
	toolDefs := l.buildToolDefs()

	// Per-loop progress. The runner is the source of truth for the live
	// counters; the operator's controller does not double-write them.
	turnCount := int32(0)
	inputTokens := int64(0)
	outputTokens := int64(0)
	toolCallCount := int32(0)
	// lastToolCatalogDigest carries the offered-tool digest across turns so a
	// catalog that did not move writes no row. Loop-local rather than a Loop
	// field: it is per-Run state, and a restarted runner re-deriving it from
	// turn 0 is correct — the re-record is byte-identical and idempotent.
	var lastToolCatalogDigest string

	if l.ProgressPublish != nil {
		l.progress = newProgressReporter(l.ProgressPublish, progressActivationDelay, progressMinInterval, time.Now)
		// Heartbeat: advance the elapsed clock every minInterval during active
		// work (tool/skill/IO) even when no tokens flow. It freezes while parked
		// on a human (pause/resume around approval waits) and ends with the turn.
		hbCtx, hbCancel := context.WithCancel(ctx)
		defer hbCancel()
		go l.progress.runHeartbeat(hbCtx, progressMinInterval)
	}
	// Operation-activity heartbeat: the live operation-subtree snapshot, on
	// the same cadence as turn-progress but gated independently
	// (Operations + OperationActivityPublish), so a session without either
	// wired incurs no extra goroutine. Unlike progress, this stream needs no
	// pause/resume of its own — channelsd's watchdog machine suppresses
	// forwarding while yielded. See runOperationActivityHeartbeat's doc.
	if l.OperationActivityPublish != nil && l.Operations != nil {
		opActCtx, opActCancel := context.WithCancel(ctx)
		defer opActCancel()
		go l.runOperationActivityHeartbeat(opActCtx, progressMinInterval)
	}
	// Periodic run-duration flush runs independently of progress publishing:
	// headless / non-channel sessions have a nil ProgressPublish but still must
	// persist accrued run-time so a long active turn that crashes cannot reset
	// the maxDuration budget on restart. Own context so it ends with Run. The
	// authoritative flush is at OnAwaitYield (the sleep boundary); this only
	// bounds run-time loss on an abrupt crash mid-turn.
	if l.RunClock != nil && l.Status != nil {
		// Final flush on ANY Run exit. agent_work_complete -> WriteIdle (and the
		// terminal writes) return without an OnAwaitYield, so without this the last
		// turn's run-time is lost before the pod exits and the next pod re-seeds
		// from a stale status.runDuration -- maxDuration would never accumulate
		// across the common channel turn path. Detached context so a graceful
		// shutdown cancel of ctx doesn't defeat the persist. Registered before the
		// flusher's cancel defer so (LIFO) the periodic flusher stops first.
		defer l.flushRunDuration(context.WithoutCancel(ctx))
		flushCtx, flushCancel := context.WithCancel(ctx)
		defer flushCancel()
		go l.runDurationFlusher(flushCtx, runDurationFlushInterval)
	}

	// Session-context the meta tools see. agent_work_complete writes here via
	// SubmitResult; the runner reads it after the loop ends.
	var (
		resultMu     sync.Mutex
		resultPosted *tool.AgentResult
	)
	// resultMu is technically redundant — dispatchToolUses' WaitGroup already
	// gives the necessary happens-before — but keeps the synchronization explicit.
	// sessionCtxMu guards this pointer-reassignment + in-place field mutation
	// against the NATS-goroutine reader in appToolSessionContext: a browser
	// widget's app-tool call can race the very entry that (re)establishes
	// SessionContext for this Run invocation.
	l.sessionCtxMu.Lock()
	sess := l.SessionContext
	if sess == nil {
		sess = &tool.SessionContext{Namespace: l.SessionKey.Namespace, Name: l.SessionKey.Name}
	}
	// SubmitResult is always bound here at Run-time so it captures this
	// invocation's resultPosted pointer. Callers that pre-populate
	// SessionContext should leave SubmitResult nil; we always overwrite it.
	sess.SubmitResult = func(r tool.AgentResult) {
		resultMu.Lock()
		rr := r
		resultPosted = &rr
		resultMu.Unlock()
	}
	if l.Mem != nil {
		sess.Mem = l.Mem
	}
	if l.KG != nil {
		sess.KG = l.KG
	}
	// Make the resolved context canonical so emitActivity (and the
	// approval host) reach the same plans Store the tool dispatch uses.
	l.SessionContext = sess
	l.sessionCtxMu.Unlock()

	// A resume with NOTHING NEW must not take a turn.
	//
	// A parked session's transcript ends with the agent's own last reply.
	// Replaying it and calling the model anyway sends a conversation ending in an
	// assistant message, which providers reject outright ("This model does not
	// support assistant message prefill. The conversation must end with a user
	// message."), and the runner exits into AwaitingRetry where every retry
	// reproduces it — for a turn with nothing to respond to.
	//
	// Reachable because a wake can arrive with no message attached: channelsd
	// wakes a session only when it has something to deliver, but a wake requested
	// to serve an agent-defined UI's data bindings (pkg/web/webui/agentui's
	// requestWake) does not.
	//
	// So park exactly where the agent would have — the same await_user_message
	// wait, off the same InboundCh, under the same idle TTL, reusing the one
	// mechanism that governs how long a runner lives. Once something DOES arrive,
	// fall through and drain it into the turn below.
	if resumedWithNothingToDo(hadInitialPrompt, messages) {
		slog.Default().Info("resume with an empty inbox: parking without a turn (nothing to respond to)",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "messages", len(messages))
		if !l.parkUntilInbound(ctx, sess) {
			// The wait ended without new input — the idle TTL elapsed, or the
			// context was cancelled. Both mean this runner is done, and it must
			// leave through the SAME door the ordinary await-idle exit uses: emit
			// the idle lifecycle signal and audit event, fire SessionEnd, and call
			// idleWithWakeRecheck, which writes the Idle phase AND re-checks the
			// inbox for a message that landed during the transition. A bare
			// `return nil` instead leaves the session at Pending forever,
			// reporting busy for a runner that had finished, and drops any message
			// that raced the park.
			if l.ChannelAttached {
				// Completion gate, resume-park exit. A settle is a settle even
				// when no turn ran: a round that ended earlier still owing
				// something reaches its resting phase HERE, and the model is not
				// in the loop to be told. Refusing enters the loop instead of
				// exiting — waking the agent to finish or to say why is exactly
				// what is owed — and costs a turn only when something is
				// genuinely outstanding.
				if refusal, blocked := l.completionSettleRefusal(ctx, sess); blocked {
					l.noteCompletionSettleBlocked(ctx, "resume park expiry")
					messages = append(messages, refusalTurn(refusal))
				} else {
					l.emitLifecycleSignal(ctx, lifecycle.SigSessionIdle, map[string]string{"reason": "idle"})
					l.emitLifecycleEvent(ctx, idleYieldEventFor(false))
					l.fireSessionEnd(ctx, "idle")
					return l.idleWithWakeRecheck(ctx, spiceboxv1alpha1.ReasonAgentSessionAwaitingUserMsg)
				}
			} else {
				// Not channel-attached: nothing can deliver a message here, so
				// there is no idle-and-wakeable state to record — the run is simply
				// over, with no terminal write to gate. parkUntilInbound already
				// declines to wait at all for such a session; this is the belt to
				// that braces.
				return nil
			}
		}
		if placed, drained, derr := l.drainInbox(ctx, nextIndex); derr != nil {
			return l.fail(ctx, spiceboxv1alpha1.ReasonAgentSessionMemoryDown, fmt.Sprintf("drain inbox after park: %v", derr))
		} else {
			for _, t := range placed {
				messages = append(messages, llm.Message{Role: "user", Content: contentBlocksFromMemory(t.Content)})
				l.maybeEchoAnnotationTurn(ctx, t)
			}
			nextIndex = drained
		}
	}

	// hasOpening decides whether this session's status.pinnedMessage projection
	// exists at all: only an input kind that posts a live opening line of its
	// own (channelkinds.TriggerDescriber, same gate opening_summary's Offer
	// uses) has one to keep current. Resolved once — the input channel kind is
	// fixed for the session's lifetime — from l.ChannelKind, which
	// internal/cmd/runner sets from spec.inputChannel.kind (empty, and so never
	// a describer, for a kubectl-driven session).
	_, hasOpening := chregistry.TriggerDescriberFor(l.ChannelKind)

	for {
		// Turn (re)active — clears any prior paused banner. Deduped, so this is
		// a no-op unless the previous emitted state was paused.
		l.emitActivity(ctx, false, "")

		if reason := l.Budget.Check(turnCount, inputTokens, outputTokens); reason != "" {
			return l.fail(ctx, budgetFailReason(reason), reason)
		}

		// Mid-session tool re-synthesis: consult the refresher for tools that
		// became available since the last turn (newly-ready secret-gated
		// separate-pod sidecars). New tools are appended to l.Tools and toolDefs
		// is rebuilt so the LLM can call them THIS turn. Errors are logged, not
		// fatal — a transient probe failure must not stall the live conversation;
		// the refresher is consulted again next turn.
		if l.ToolRefresher != nil {
			refreshed, rerr := l.ToolRefresher(ctx)
			if rerr != nil {
				slog.Default().Info("tool refresher errored; proceeding with existing tool set",
					"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "err", rerr.Error())
			}
			// Advisories are surfaced even when the pass also errored: they are
			// collected before the failing step, and a sidecar that will never
			// come up is exactly what the agent must be told about.
			for _, adv := range refreshed.Advisories {
				appendTransientAdvisory(&messages, adv)
				slog.Default().Info("mid-session tool advisory injected",
					"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "advisory", adv)
			}
			if added := refreshed.Added; rerr == nil && len(added) > 0 {
				// Replacement semantics: a refresher-returned tool whose name
				// collides with an already-live tool REPLACES it, keeping the
				// per-turn buildToolDefs / dispatchToolUses rebuilds pointed at the
				// live pod. A secret-gated sidecar replaced mid-session (token
				// rotation) re-emits the same `<ref>_<tool>` names against a new IP,
				// so leaving the stale entries would make buildToolDefs emit
				// duplicate defs and dispatchToolUses' byName map silently shadow
				// one.
				// The lock guards the reassignment (and applyToolRefresh's own read
				// of l.Tools) against the off-loop lookupTool reader in the NATS
				// app-tool handler; the loop-goroutine readers below are sequential
				// with this write and need none.
				l.toolsMu.Lock()
				l.Tools = applyToolRefresh(l.Tools, added)
				l.toolsMu.Unlock()
				toolDefs = l.buildToolDefs()
				names := make([]string, 0, len(added))
				for _, t := range added {
					names = append(names, t.Name())
				}
				slog.Default().Info("mid-session tool re-synthesis: added/replaced tools",
					"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
					"count", len(added), "tools", strings.Join(names, ","))
			}
		}

		var onEvent func(llm.StreamEvent)
		if l.OnStreamEvent != nil || l.progress != nil {
			onEvent = l.streamSink
		}
		// Hydration builds THIS request's view of the conversation: a copy whose
		// attachment refs are resolved into native blocks. The refs are
		// deliberately not written back to `messages`, so every request re-derives
		// the window, the byte budget, and the head-turn boundary from the refs
		// themselves (see attachments.go).
		//
		// The copy is shallow: markCacheBreakpoints below writes Cacheable
		// straight through into blocks `messages` still shares. That is benign
		// only because markCacheBreakpoints clears every Cacheable UNCONDITIONALLY
		// before marking; an incremental-marking optimization that dropped the
		// clear would turn this shallow copy into an accumulator, piling marks up
		// on shared blocks past the provider's breakpoint budget.
		//
		// MUST precede markCacheBreakpoints: breakpoints are placed on the LAST
		// block of a message and hydration adds and removes blocks, so marking
		// first would anchor the cache to a block that no longer exists at send
		// time. Hydration never adds or removes MESSAGES, so l.prevBreakpointMsg —
		// an index carried across requests — stays valid against it.
		hydrated := l.hydrateAttachments(ctx, messages)
		// Also MUST precede markCacheBreakpoints, same reason: this appends a
		// block to the most recent user message, so marking first would anchor
		// the breakpoint to a block that no longer exists at send time.
		hydrated = l.hydrateSpeakerProfile(ctx, hydrated)
		l.markCacheBreakpoints(hydrated)
		// A whole-session REPLAY offers the tool set its captured run recorded
		// for this turn, not the one this fixture happens to compose. nil in
		// production — see Loop.ReplayToolCatalog — where turnToolDefs is
		// toolDefs and nothing below can tell the difference.
		turnToolDefs := toolDefs
		if l.ReplayToolCatalog != nil {
			turnToolDefs = l.replayToolDefs(nextIndex, toolDefs)
		}
		req := llm.Request{
			Model:           l.Model,
			System:          []llm.SystemBlock{{Text: l.System, Cacheable: true}},
			Messages:        hydrated,
			Tools:           turnToolDefs,
			MaxTokens:       l.MaxTokens,
			OnEvent:         onEvent,
			UserID:          l.UserID,
			ProviderRouting: l.Routing,
		}
		// Record which tools this Send actually offered, when the set moved.
		//
		// FromTurnIndex is nextIndex — the transcript index the assistant turn
		// THIS Send produces will land at — not turnCount. turnCount resets to
		// 0 on every Run() invocation (see the comment on nextIndex a few dozen
		// lines below: it is rebuilt by replay() across restarts and increases
		// monotonically, unlike turnCount), and the entry id is
		// toolcat-<FromTurnIndex> on an append-only Kind, so a turnCount-keyed
		// id would collide with turn 0 on every resumed Run and the conflict
		// would be swallowed as "already recorded" — silently dropping the
		// catalog for the entire resumed portion of the session. nextIndex also
		// matches the axis a capture reads back on: it walks memory.Turn by
		// .Index and resolves via ForTurn(ctx, m, scope, thatIndex).
		//
		// Best-effort and logged, never fatal: a session must not fail because
		// its evidence trail could not be written, and the failure is loud
		// enough to find (AGENTS.md — never silently drop an error).
		if l.LifecycleMemory != nil {
			names := make([]string, 0, len(turnToolDefs))
			for _, td := range turnToolDefs {
				names = append(names, td.Name)
			}
			if digest, changed := toolcatalog.Changed(lastToolCatalogDigest, names); changed {
				if err := toolcatalog.Record(ctx, l.LifecycleMemory, l.bindingScope(), toolcatalog.Content{
					FromTurnIndex: nextIndex,
					Tools:         names,
				}); err != nil {
					slog.Default().Info("toolcatalog.Record failed; the session runs but its offered tools are unrecorded",
						"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
						"turnIndex", nextIndex, "err", err.Error())
				} else {
					lastToolCatalogDigest = digest
				}
			}
		}
		l.progressf("· thinking…")
		// Turn-boundary marker. A long single-turn generation (huge context →
		// slow/large response) otherwise looks like a frozen runner: the loop
		// blocks in Provider.Send with no log and no status change for minutes.
		// This line lets an operator see the turn began, with how close the
		// session already is to its token budget — if it's the last log line,
		// the runner is waiting on the LLM, not deadlocked.
		slog.Default().Info("turn: calling LLM",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
			"turn", turnCount+1, "messages", len(messages), "tools", len(turnToolDefs),
			"priorInputTokens", inputTokens, "priorOutputTokens", outputTokens,
			"maxTokens", l.MaxTokens)
		// Wrap Send's ctx so Interrupt can cancel just this in-flight LLM call
		// (via l.interrupts.fire()) without tearing down the turn's ctx. The
		// registry is cleared unconditionally right after Send returns, on
		// both the success and error paths, so a stale providerCancel can
		// never be fired against a completed call.
		sendCtx, sendCancel := context.WithCancelCause(ctx)
		l.interrupts.setProvider(sendCancel)
		resp, err := l.Provider.Send(sendCtx, req)
		l.interrupts.clearProvider()
		sendCancel(nil)
		if err != nil {
			if context.Cause(sendCtx) == errInterruptedByUser {
				// The user interrupted the LLM call to jump the queue. Not a
				// provider failure — drain the held queue and continue.
				placed, cont, derr := l.drainAfterInterrupt(ctx, nextIndex)
				if derr != nil {
					// Tolerate a drain error here (matches the sibling drain
					// sites): the Send was cancelled either way, so retry next
					// iteration rather than hard-failing the session.
					slog.Default().Info("post-interrupt drain (send) errored; message stays held, retrying next iteration",
						"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "err", derr.Error())
				} else if cont {
					for _, tn := range placed {
						messages = append(messages, llm.Message{Role: "user", Content: contentBlocksFromMemory(tn.Content)})
					}
					nextIndex += len(placed)
				}
				continue
			}
			// A request that carried a native attachment block may have been
			// rejected BECAUSE of it: the declared MIME is a hint, not a verified
			// fact, and only the provider knows what bytes it will accept. Suppress
			// native blocks for the session and retry the turn once before treating
			// this as a provider failure.
			//
			// Without this the failure is permanent, not transient: an attachment
			// with no text fallback lives in the persistent window, so hydration
			// rebuilds the identical block every request — AwaitingRetry's Retry
			// button reproduces the rejected payload until the retry budget drains,
			// and a restart replays the durable turn. One malformed screenshot ends
			// the conversation.
			//
			// nativeSuppressed bounds this to a single extra call: the retry cannot
			// carry a native block, so it cannot re-enter this branch.
			if !l.nativeSuppressed.Load() && requestHasNativeBlock(req) {
				l.nativeSuppressed.Store(true)
				slog.Default().Info("provider error on a request carrying a native attachment block; suppressing native blocks and retrying once",
					"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
					"err", err.Error())
				continue
			}
			return l.failProviderError(ctx, err.Error())
		}

		// Surface any text the LLM emitted alongside (or before) tool
		// calls so the local-mode driver can stream it to the user.
		for _, b := range resp.Content {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				l.progressf("%s", strings.TrimSpace(b.Text))
			}
		}

		// Capture the durable memory index BEFORE appending the assistant turn.
		// nextIndex is rebuilt by replay() across runner restarts, so it increases
		// monotonically across resume — unlike turnCount, which resets to 0 on
		// each Run() invocation. dispatchToolUses threads it through so tool-use
		// goroutines can stamp Envelope.Seq via PackSeq(assistantTurnIndex,
		// blockIndex).
		assistantTurnIndex := nextIndex
		// Keep the emit-time fallback index in lock-step with the turn the loop
		// is about to dispatch, so a loop-side pause emitted for this turn carries
		// the same memTurnIndex the tool blocks stamp.
		l.setLastAssistantTurnIndex(assistantTurnIndex)
		assistantContent := contentBlocksToMemory(resp.Content)
		servedDisplay := servedModelDisplay(l.Provider.Name(), resp.Model, l.Model)
		assistantTurn := memory.Turn{
			Index: nextIndex, Role: "assistant",
			Content:   assistantContent,
			CreatedAt: time.Now().UTC(),
			Refused:   resp.StopReason == "refusal",
			Model:     servedDisplay,
			Usage: &memory.Usage{
				InputTokens:         resp.Usage.InputTokens,
				OutputTokens:        resp.Usage.OutputTokens,
				CacheCreationTokens: resp.Usage.CacheCreationTokens,
				CacheReadTokens:     resp.Usage.CacheReadTokens,
			},
		}
		if err := l.Memory.Append(ctx, assistantTurn); err != nil {
			return l.fail(ctx, spiceboxv1alpha1.ReasonAgentSessionMemoryDown, fmt.Sprintf("append assistant turn: %v", err))
		}
		// Bare model for the per-model cost bucket: the SAME inputs
		// servedModelDisplay used (resp.Model, falling back to l.Model), not a
		// re-parse of servedDisplay — see modelUsageBucket's doc.
		bareServedModel := resp.Model
		if bareServedModel == "" {
			bareServedModel = l.Model
		}
		l.addModelUsage(servedDisplay, bareServedModel, resp.Usage)
		l.emitLifecycleSignal(ctx, lifecycle.SigTurnCompleted, map[string]string{
			"turnIndex": strconv.Itoa(assistantTurn.Index),
			"role":      assistantTurn.Role,
		})
		// Typed transition event for the lifecycle state machine log (distinct
		// from the SigTurnCompleted signal above, which drives operator-side
		// hooks like KG ingestion).
		l.emitLifecycleEvent(ctx, lifecyclecore.TurnCompleted{})
		messages = append(messages, llm.Message{Role: "assistant", Content: resp.Content})
		nextIndex++

		turnCount++
		inputTokens += resp.Usage.InputTokens
		outputTokens += resp.Usage.OutputTokens
		l.progress.commitCall(resp.Usage.InputTokens, resp.Usage.OutputTokens)
		l.addUsage(resp.Usage) // cumulative snapshot incl. cache buckets

		// We don't know yet how many tool_use blocks will dispatch successfully,
		// but the model emitted them as part of THIS turn — count them now so
		// progress reflects the current turn's work.
		toolCallCount += int32(len(resp.ToolUses()))

		snap := l.usageSnapshot()
		besteffort.Log(slog.Default().Info, "patch session progress",
			l.Status.PatchProgress(ctx, Progress{
				TurnCount: turnCount, InputTokens: inputTokens, OutputTokens: outputTokens,
				CacheCreationTokens: snap.CacheCreationTokens, CacheReadTokens: snap.CacheReadTokens,
				ToolCallCount: toolCallCount,
			}),
			"turnCount", turnCount, "outputTokens", outputTokens)

		// A content-policy refusal is terminal for THIS turn regardless of
		// whether tool_use blocks were emitted: the turn is untrustworthy, so we
		// drop its tool_uses undispatched and park for a clean retry.
		if resp.StopReason == "refusal" {
			return l.handleRefusal(ctx)
		}

		// Terminal: no tool_use.
		uses := resp.ToolUses()
		// Classify the LLM's stop_reason once. An unexpected stop_reason
		// either gets injected into the tool_results as an advisory (if
		// there's anywhere to attach it) or becomes the session's failure
		// reason (when no tool_use was emitted to attach it to).
		stopAdvisory := stopReasonAdvisory(resp.StopReason, l.MaxTokens)
		if len(uses) == 0 {
			if stopAdvisory != "" {
				// Unexpected stop_reason (max_tokens, stop_sequence, pause_turn, or
				// an unrecognized value; `refusal` is intercepted above and never
				// reaches this branch) AND no tool_use to attach the recovery
				// advisory to. The advisory is AGENT-facing guidance and must NEVER
				// become the user-visible failure message — that leaks raw
				// "[runner-warning] …" text into the chat. Keep it in operator
				// surfaces (log + runner note for `kubectl describe agentsession`)
				// and fail with a CLEAN, user-facing message.
				slog.Default().Info("terminal stop_reason with no tool_use",
					"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name,
					"stopReason", resp.StopReason, "advisory", stopAdvisory)
				besteffort.Log(slog.Default().Info, "AppendRunnerNote (terminal stop_reason)",
					l.Status.AppendRunnerNote(ctx, "terminal stop_reason "+resp.StopReason+": "+stopAdvisory),
					"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name)
				return l.fail(ctx, spiceboxv1alpha1.ReasonAgentSessionProviderErr, userFacingStopReason(resp.StopReason))
			}
			// Expected end_turn / empty stop_reason: a "stalled" model in a chat
			// context is a protocol violation on our side, not a user-actionable
			// failure — recover silently to Idle rather than posting "Agent
			// failed: Stalled" on every protocol slip, often while respond_to_user
			// has already delivered an answer. A genuinely broken agent surfaces
			// through the channelsd status watchdog's 30s warning + 60s timeout,
			// which key off real user-visible silence. The runner note keeps the
			// recovery visible in `kubectl describe agentsession` even though the
			// channel stays clean.
			//
			// A DELEGATED child is the exception, and it takes the kubectl
			// terminal below rather than the Idle one. The silent recovery is
			// silent because a person can simply say something else and wake the
			// session; a delegated child has nobody to do that. Its
			// SubagentRequest would stay Running — reconcileChild resolves only on
			// a terminal child phase — so the parent's delegate /
			// reply_to_subagent call polls to its own 30-minute ceiling. Nothing
			// was delivered either: respond_to_user is withheld from such a
			// session (its binding reaches an agent, not a person), so a turn with
			// no tool_use produced no answer to park on top of.

			// Completion gate, no-tool_use exit. Ahead of BOTH endings below
			// (the Idle yield and the loud fail), because a settle it refuses is
			// not an ending at all. This is the
			// exit no per-tool check could ever cover: the model called nothing,
			// so there is no call to intercept and no tool_result to answer with.
			// The refusal is spliced in as a user-role message instead, and the
			// loop takes another turn — for a delegated child too, whose class
			// promised the same thing and whose refusal names return_result.
			if l.ChannelAttached {
				if refusal, blocked := l.completionSettleRefusal(ctx, sess); blocked {
					l.noteCompletionSettleBlocked(ctx, "turn with no tool_use")
					messages = append(messages, refusalTurn(refusal))
					continue
				}
			}
			noteLabel, note := "AppendRunnerNote (no-tool_use auto-Idle)",
				"model emitted no tool_use; auto-completed to Idle (channel-attached recovery)"
			if l.DelegatedChild {
				noteLabel, note = "AppendRunnerNote (no-tool_use delegation failure)",
					"model emitted no tool_use; failed the delegation instead of parking Idle, so the waiting parent learns now rather than at its poll timeout"
			}
			besteffort.Log(slog.Default().Info, noteLabel,
				l.Status.AppendRunnerNote(ctx, note),
				"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name)
			if l.ChannelAttached && !l.DelegatedChild {
				l.emitLifecycleSignal(ctx, lifecycle.SigSessionIdle, map[string]string{"reason": "idle"})
				// Channel-attached recovery yield to Idle (pod exits, respawn on wake).
				l.emitLifecycleEvent(ctx, lifecyclecore.IdleYield{})
				l.fireSessionEnd(ctx, "idle")
				return l.idleWithWakeRecheck(ctx, spiceboxv1alpha1.ReasonAgentSessionAgentWorkComplete)
			}
			// kubectl-driven, or a delegated child: keep the loud failure — that
			// channel's UX is exit codes + status conditions (or, for a
			// delegation, the parent's tool result), no chat notification
			// involved.
			// Names the tool THIS session was offered: for a delegated child
			// that is return_result, and this message is the only text
			// reconcileChild carries back to the parent.
			return l.fail(ctx, spiceboxv1alpha1.ReasonAgentSessionStalled,
				"model emitted no tool_use; "+l.terminalToolName()+" was not called")
		}

		// Open a per-turn pt-tag ledger: the PostToolCall mint records each
		// datum's tag here, and the result-wrap below reads it to emit <pt>
		// markup for the model. Scoped to this turn's dispatch+wrap, and inert
		// when the fine-grained capability is off — nothing mints, so nothing
		// wraps.
		ctx, _ = provenance.WithTagLedger(ctx)

		// Dispatch concurrently. resp.Content carries the assistant turn's
		// blocks (text + tool_uses, in order); dispatchToolUses uses this
		// to harvest a per-tool_use justification for the approval prompt.
		results, terminal := l.dispatchToolUses(ctx, uses, sess, turnCount, assistantTurnIndex, delivered, resp.Content)

		// Recompute status.pinnedMessage now that this turn's tool calls — which
		// may have set the opening-summary body (update_opening_summary) or
		// concluded the trigger (conclude_trigger_status) — have all completed.
		// Placed here rather than at the PatchProgress site above (which runs
		// BEFORE dispatch) so a turn that concludes AND yields/completes in the
		// same turn still gets one fresh patch before any of the returns below —
		// several of which exit the loop for good. Only-on-change: most turns
		// write nothing. Best-effort: a stale badge is a display nit, never a
		// reason to fail a working session.
		besteffort.Log(slog.Default().Info, "recompute pinned message",
			l.recomputeAndPatchPinnedMessage(ctx, sess, hasOpening),
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name)

		// An unexpected stop_reason (max_tokens, stop_sequence, pause_turn, …;
		// `refusal` is intercepted earlier and never reaches dispatch) means the
		// tool_use args just dispatched may be truncated, often arriving as empty
		// `{}`. Without an explicit advisory the model sees something like "text
		// is required" and retries the same empty call, burning turns; prepending
		// the note and flagging IsError makes it a failure to correct rather than
		// a successful step.
		if stopAdvisory != "" {
			for i := range results {
				results[i].Content = stopAdvisory + "\n\n" + results[i].Content
				results[i].IsError = true
				// An advisory is never a terminal signal — clear any
				// Terminal/IdleExit the underlying tool may have set (an
				// empty agent_work_complete would otherwise short-circuit
				// the recovery loop).
				results[i].Terminal = false
				results[i].IdleExit = false
				results[i].ShareDenied = false
				// AwaitResumed is deliberately NOT cleared: a truncated *assistant*
				// response is orthogonal to whether a *user* message arrived.
				// A parking tool's Execute (await_user_message, ask_parent) sets
				// it off a real InboundCh wake, so clearing it would skip the
				// yield-boundary drain below and strand the reply.
			}
			terminal = false
		}

		// Completion gate, yield exit. A Terminal+IdleExit result parks the
		// session at Idle a few lines below without the terminal tool — and so
		// without its gate — ever running: await_user_message's TTL expiry or
		// cancellation, and respond_to_user's share-denied yield, all end the
		// round through that door. Checked here rather than at the park itself
		// because the refusal has to reach the model as a tool_result, and the
		// blocks below are built from `results` exactly once.
		//
		// resultPosted is the skip: the terminal tool — agent_work_complete, or
		// return_result for a delegated child; both submit through SubmitResult —
		// ran in this same turn and its own gate already allowed the settle (met,
		// or bypassed with a reason the user was told). Re-refusing there would
		// demand a second bypass for the one the model just gave. Without the
		// skip, a turn calling the terminal tool AND await_user_message together
		// would also be double-charged for one decision.
		if terminal && l.ChannelAttached {
			resultMu.Lock()
			gateAlreadyRan := resultPosted != nil
			resultMu.Unlock()
			if !gateAlreadyRan {
				if refusal, blocked := l.completionSettleRefusal(ctx, sess); blocked {
					l.noteCompletionSettleBlocked(ctx, "tool yield to idle")
					for i := range results {
						if !results[i].Terminal {
							continue
						}
						// Prepended, not replaced: a share-denied yield's own text
						// says the send was refused and must not be retried, which
						// the model still needs in order to choose the bypass over
						// another respond_to_user.
						results[i].Content = refusal + "\n\n" + results[i].Content
						results[i].IsError = true
						results[i].Terminal = false
						results[i].IdleExit = false
						results[i].ShareDenied = false
					}
					terminal = false
				}
			}
		}

		// Build user turn (tool_results). Every result's Content is wrapped in
		// untrusted-data delimiters before being fed back to the LLM, so a
		// compromised MCP server or attacker-controlled tool output cannot smuggle
		// instructions: the system-prompt rule (ComposeSystem in prompt.go)
		// teaches the model to treat everything between the markers as data to
		// observe, never as commands. All tool results — sandbox, MCP, and meta —
		// are wrapped uniformly; the markers are harmless on in-process results.
		// sessionUID backs applyUIResource's ArtifactRender owner reference
		// (mirrors artifact_prepare.go's sess.AgentSessionUID != "" guard); empty
		// when Loop.AgentSession is unset (kubectl-driven/tests), which
		// applyUIResource treats as "no owner ref" rather than an error.
		var sessionUID types.UID
		if l.AgentSession != nil {
			sessionUID = l.AgentSession.UID
		}
		// l.Status is a *StatusPatcher; only assign it into the
		// uiResourceStatusWriter interface field when non-nil. Assigning a
		// nil *StatusPatcher directly would produce a non-nil interface
		// wrapping a nil pointer (AGENTS.md's typed-nil-interface gotcha) —
		// applyUIResource's `deps.Status != nil` guard would then pass and
		// the subsequent method call would panic on the nil receiver.
		var statusWriter uiResourceStatusWriter
		if l.Status != nil {
			statusWriter = l.Status
		}
		uiDeps := uiResourceDeps{Client: l.Client, Artifacts: l.Artifacts, Status: statusWriter}

		toolResultBlocks := make([]llm.ContentBlock, len(results))
		memoryBlocks := make([]memory.ContentBlock, len(results))
		for i, r := range results {
			// Divert secret-output values out-of-band BEFORE wrapping so that
			// NEITHER the LLM tool_result NOR session memory ever sees the raw
			// value — only the handle + description reach both blocks.
			r = applySecretOutput(ctx, sess.SecretOut, l.SecretOutPublisher, l.Status, l.SessionKey.Name, r, uses[i].ID)
			// UIResource and SecretOutput are independent side-band fields; order
			// relative to the secret-output diversion above doesn't matter.
			r = applyUIResource(ctx, uiDeps, l.EchoPublish, l.EnvelopeSigner, l.SessionKey.Namespace, l.SessionKey.Name, sessionUID, &l.uiEscalated, r)
			// A tagged, non-error result gets the pt-untrusted envelope (untrusted
			// framing + provenance id under one nonce); everything else gets the
			// plain nonce'd untrusted wrap. The envelope is not double-wrapped.
			wrapped, handled := ptWrapResult(ctx, uses[i].ID, r.Content, r.IsError)
			if !handled {
				wrapped = wrapUntrustedToolOutput(r.Content)
			}
			toolResultBlocks[i] = llm.ContentBlock{
				Type: "tool_result",
				ToolResult: &llm.ToolResultBlock{
					ToolUseID: uses[i].ID, Content: wrapped, IsError: r.IsError,
				},
			}
			// RenderedLive rides onto the MEMORY block only. It is a fact about
			// what the user has already seen — the read path's input, not the
			// model's — and the llm block above stays byte-identical to what
			// every provider adapter already accepts.
			memoryBlocks[i] = memory.ContentBlock{
				Type: "tool_result",
				ToolResult: &memory.ToolResultBlock{
					ToolUseID: uses[i].ID, Content: wrapped, IsError: r.IsError,
					RenderedLive: r.RenderedLive,
				},
			}
		}
		userTurn := memory.Turn{
			Index: nextIndex, Role: "user",
			Content:   memoryBlocks,
			CreatedAt: time.Now().UTC(),
		}
		if err := l.Memory.Append(ctx, userTurn); err != nil {
			return l.fail(ctx, spiceboxv1alpha1.ReasonAgentSessionMemoryDown, fmt.Sprintf("append user turn: %v", err))
		}
		messages = append(messages, llm.Message{Role: "user", Content: toolResultBlocks})
		nextIndex++

		// A PreToolCall/PostToolCall hook halted this dispatch. host.Halt
		// already wrote terminal Failed status and ran SessionEnd via l.fail;
		// stop the loop now so no further Provider.Send call happens past the
		// fail-closed halt. The tool_results above are recorded for audit.
		if l.dispatchHalted {
			return nil
		}

		if terminal {
			// Channel-attached IdleExit: an await_user_message / ask_parent TTL, context cancelled,
			// or the PreResponse share-denied yield respond_to_user routes here as
			// a Terminal+IdleExit result. All resolve to the same phase (Idle) but
			// record distinct lifecycle audit events — a share-denied yield logs
			// ShareDeniedYield so the audit trail can tell it from a plain
			// await-idle; everything else logs IdleYield.
			if l.ChannelAttached {
				for _, r := range results {
					if r.Terminal && r.IdleExit {
						l.emitLifecycleSignal(ctx, lifecycle.SigSessionIdle, map[string]string{"reason": "idle"})
						l.emitLifecycleEvent(ctx, idleYieldEventFor(r.ShareDenied))
						l.fireSessionEnd(ctx, "idle")
						return l.idleWithWakeRecheck(ctx, spiceboxv1alpha1.ReasonAgentSessionAwaitingUserMsg)
					}
				}
			}

			// The terminal tool was called: agent_work_complete, or return_result
			// for a delegated child — both submit through SubmitResult, which
			// populated `resultPosted`.
			resultMu.Lock()
			r := resultPosted
			resultMu.Unlock()
			if r == nil {
				return l.fail(ctx, spiceboxv1alpha1.ReasonAgentSessionProviderErr, "tool returned Terminal=true without SubmitResult")
			}
			// A DELEGATED child is channel-attached and still completes here.
			// Its binding reaches its parent, not a person, so there is no
			// conversation to keep alive: the delegation is what was talking,
			// and reply_to_subagent refuses a request that is not
			// AwaitingParent, so nothing further can arrive on it. Parking
			// Idle instead leaves the SubagentRequest Running forever —
			// reconcileChild resolves only on Succeeded, and only Succeeded
			// carries status.result back — so the parent's own delegate /
			// reply_to_subagent call polls to its timeout and no task/chat
			// delegation can ever finish.
			if l.ChannelAttached && !l.DelegatedChild {
				// A message queued while the agent worked means the user gave it
				// more to do: drain the held inbox and continue the loop instead
				// of parking Idle (which would strand the message until an
				// external wake). Empty queue → the normal Idle park below.
				placed, drained, derr := l.drainHeldAtYield(ctx, nextIndex)
				if derr != nil {
					// Don't drop the failure silently: the message stays held as an
					// undrained inbox turn and re-drains on the next wake, but an
					// operator must be able to see the drain attempt failed.
					slog.Default().Info("drain inbox (work-complete) errored; parking Idle, held message re-drains on next wake",
						"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "err", derr.Error())
				} else if len(placed) > 0 {
					for _, tn := range placed {
						messages = append(messages, llm.Message{Role: "user", Content: contentBlocksFromMemory(tn.Content)})
					}
					nextIndex = drained
					continue // re-enter the loop with the drained turns
				}
				// Channel-attached: agent_work_complete keeps the session alive in
				// Idle (the conversation persists; subsequent inbounds wake the
				// session). The summary is already in memory + published to the
				// channel as the final assistant message.
				l.emitLifecycleSignal(ctx, lifecycle.SigSessionIdle, map[string]string{"reason": "idle"})
				l.emitLifecycleEvent(ctx, lifecyclecore.AgentWorkComplete{Kubectl: false})
				l.fireSessionEnd(ctx, "idle")
				return l.idleWithWakeRecheck(ctx, spiceboxv1alpha1.ReasonAgentSessionAgentWorkComplete)
			}
			l.emitLifecycleSignal(ctx, lifecycle.SigSessionCompleted, map[string]string{"outcome": "succeeded"})
			// A kubectl session, or a delegated child: agent_work_complete →
			// Succeeded, which is what carries status.result to whoever is
			// waiting on it.
			l.emitLifecycleEvent(ctx, lifecyclecore.AgentWorkComplete{Kubectl: true})
			l.fireSessionEnd(ctx, "completed")
			return l.Status.WriteSucceeded(ctx, spiceboxv1alpha1.AgentResult{
				Summary:   r.Summary,
				Artifacts: convertArtifacts(r.Artifacts),
			})
		}

		// A user Interrupt fired during this dispatch: the cancelled tools left
		// synthesized results above; now drain the queued messages the user
		// jumped ahead to, and continue. (An interrupt sets no terminal result,
		// so control reaches here normally.)
		if placed, cont, derr := l.drainAfterInterrupt(ctx, nextIndex); derr != nil {
			slog.Default().Info("post-interrupt drain (dispatch) errored; message stays held, retrying next iteration",
				"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "err", derr.Error())
		} else if cont {
			for _, tn := range placed {
				messages = append(messages, llm.Message{Role: "user", Content: contentBlocksFromMemory(tn.Content)})
			}
			nextIndex += len(placed)
			continue
		}

		// Yield-boundary drain: pick up queued inbound human messages ONLY when
		// the just-finished batch was a parking tool's resume. Ordinary
		// mid-tool-loop iterations HOLD the inbox (a held message is delivered at
		// the next yield, never spliced into the middle of the agent's work).
		if l.ChannelAttached && resultsIncludeAwaitResume(results) {
			// drainAwaitResume (not the bare drainHeldAtYield) so a resume that
			// reads an empty held-inbox retries for read-after-write visibility
			// instead of silently stranding the just-arrived reply.
			placed, drained, derr := l.drainAwaitResume(ctx, nextIndex)
			if derr != nil {
				// Tolerate a drain error here (as the sibling drain sites
				// do): the next loop iteration retries the drain. Log it
				// with context rather than dropping it silently.
				slog.Default().Info("drain inbox (tail) errored; will retry next iteration",
					"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "err", derr.Error())
			} else {
				for _, t := range placed {
					messages = append(messages, llm.Message{Role: "user", Content: contentBlocksFromMemory(t.Content)})
					// Mirror an annotation batch as a KindUserEcho once it's durably
					// placed; never aborts the drain on failure (see
					// maybeEchoAnnotationTurn's doc). This is the common path for a
					// live-view annotation submitted while the session is running.
					l.maybeEchoAnnotationTurn(ctx, t)
				}
				nextIndex = drained
			}
		}

		// The live-park counterpart of the Run-start slot delivery. A child
		// that asked for data and got a fast answer resumes HERE and never
		// restarts, so without this it continues holding its parent's reply
		// and not the datum that reply is about.
		//
		// Deliberately OUTSIDE the ChannelAttached gate above: that gate is
		// about draining a human's queued messages, and a delegated child
		// generally has no channel at all — which is precisely the session
		// this delivery exists for.
		if resultsIncludeAwaitResume(results) {
			// awaitBind unconditionally here: reaching this line means a
			// parking tool was woken by a real answer, which is exactly the
			// in-flight case. The Run-start site can distinguish a wake that
			// carried nothing; this one cannot, and waiting briefly for a
			// binding that never comes is the cheaper error.
			if blocks := l.deliverNewBoundSlots(ctx, prior, true); len(blocks) > 0 {
				messages = append(messages, llm.Message{Role: "user", Content: contentBlocksFromMemory(blocks)})
			}
		}

		// Post-dispatch budget check: catches budget exhaustion after tool
		// results have been appended and before the next LLM call.
		if reason := l.Budget.Check(turnCount, inputTokens, outputTokens); reason != "" {
			return l.fail(ctx, budgetFailReason(reason), reason)
		}
	}
}

func convertArtifacts(in []tool.ResultArtifact) []spiceboxv1alpha1.ResultArtifact {
	out := make([]spiceboxv1alpha1.ResultArtifact, len(in))
	for i, a := range in {
		out[i] = spiceboxv1alpha1.ResultArtifact{ID: a.ID, Description: a.Description}
	}
	return out
}
