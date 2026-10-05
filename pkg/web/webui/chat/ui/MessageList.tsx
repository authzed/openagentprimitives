import { memo, useEffect, useMemo, useRef } from "react";
import { AlertCircle, CheckCircle2, Circle, Download, ExternalLink, Loader2, RotateCw, XCircle, ListChecks } from "lucide-react";
import { cn, Markdown } from "@ap/design";
import type { ChatLine, OperationActivityNode, PlanItemRef, PlanUpdateInner } from "./types";
import { OperationTree } from "./OperationTree";
import { InteractionCard } from "./InteractionCard";
import { NoticeCard } from "./NoticeCard";
import { OpeningCard } from "./OpeningCard";
import { ToolSessionBlock } from "./ToolSessionBlock";
import type { ToolSessionMap, ToolSessionState } from "./toolSession";

// MessageText renders a finalized conversation message (a user send or an
// agent reply) as GitHub-flavored markdown — tables, fenced code blocks,
// lists, links — via the shared @ap/design <Markdown>. Raw HTML is NOT
// rendered (react-markdown escapes it; no rehype-raw), so this stays safe
// against HTML/script injection from agent/user output, same as the previous
// plain-text renderer. The artifact-view chat panel uses the same component.
// Memoized: a committed message's `text` is immutable once appended, so a
// re-render of the list on every stream_delta token must NOT re-run react-markdown's
// parse for every already-committed bubble (the per-token re-parse storm).
const MessageText = memo(function MessageText({ text }: { text: string }) {
  return <Markdown>{text}</Markdown>;
});

// PlainText renders text verbatim with line breaks preserved — for system /
// error chrome and the in-progress stream, which must NOT be markdown-reflowed:
// a status note should read literally, and half-streamed tokens would flicker
// through partial markdown until the reply finalizes into a MessageText bubble.
function PlainText({ text }: { text: string }) {
  return <span className="whitespace-pre-wrap break-words">{text}</span>;
}

// isDoneStatus is the single "this plan step is terminally finished" predicate —
// the done/complete/completed synonyms plus "skipped" — shared by BOTH the
// checklist glyph and the dimmed styling so the two can't drift (they used to
// enumerate the terminal set separately and disagreed on "skipped").
function isDoneStatus(status: string): boolean {
  return status === "done" || status === "complete" || status === "completed" || status === "skipped";
}

// planItemIcon maps a plan item's status to its checklist glyph. Unknown
// statuses fall back to a neutral open circle (graceful — the plan schema may
// grow new statuses the UI hasn't special-cased yet). `live` gates the
// in-progress SPIN: a step is only actively-spinning while a turn is running;
// once the session goes idle it holds a static ring (the plan snapshot may
// still say "in_progress" because the agent yielded without marking it done).
function planItemIcon(status: string, live: boolean) {
  if (isDoneStatus(status)) {
    return <CheckCircle2 className="h-3.5 w-3.5 shrink-0 text-success" aria-hidden="true" />;
  }
  switch (status) {
    case "in_progress":
    case "active":
    case "running":
      return <Loader2 className={cn("h-3.5 w-3.5 shrink-0 text-primary", live && "animate-spin")} aria-hidden="true" />;
    case "blocked":
    case "failed":
    case "error":
      return <XCircle className="h-3.5 w-3.5 shrink-0 text-destructive" aria-hidden="true" />;
    default:
      return <Circle className="h-3.5 w-3.5 shrink-0 text-muted-foreground" aria-hidden="true" />;
  }
}

// StepList renders a flat run of plan steps with their per-status glyphs.
// Completed/skipped steps are dimmed.
function StepList({ items, live }: { items: PlanItemRef[]; live: boolean }) {
  return (
    <ul className="flex flex-col gap-1">
      {items.map((it) => {
        const dim = isDoneStatus(it.status);
        return (
          <li key={it.id} className="flex items-start gap-1.5 text-xs leading-snug">
            <span className="mt-0.5">{planItemIcon(it.status, live)}</span>
            <span className={cn("break-words", dim ? "text-muted-foreground" : "text-card-foreground")}>
              {it.label}
            </span>
          </li>
        );
      })}
    </ul>
  );
}

// PlanSteps renders a plan's steps grouped under the phases the agent declared,
// falling back to one flat list when it declared none.
//
// Grouping exists because the plan and the approval card describe the SAME
// work: the card speaks in phases, and a flat checklist beside it read as a
// second, unrelated plan. Steps whose phase matches no declared phase — and
// steps with no phase at all — are collected under a trailing group rather than
// dropped, because `phase` is a free-text display hint the agent rewrites on
// every update_plan call, so a stale or misspelled id must lose its heading,
// never its step.
//
// Every string rendered here is AGENT-AUTHORED. React escapes it; nothing keys
// on it. In particular a phase's `label` is shown as a heading for the agent's
// own staging — it is NOT the name of anything being authorized, which is why
// the approval card titles its phases by index instead.
export function PlanSteps({ plan, live }: { plan: PlanUpdateInner; live: boolean }) {
  const items: PlanItemRef[] = plan.items ?? [];
  const phases = plan.phases ?? [];
  if (phases.length === 0) return <StepList items={items} live={live} />;

  const grouped = phases
    .map((ph) => ({ phase: ph, steps: items.filter((it) => it.phase === ph.id) }))
    .filter((g) => g.steps.length > 0);
  const known = new Set(phases.map((p) => p.id));
  const ungrouped = items.filter((it) => !it.phase || !known.has(it.phase));

  return (
    <div className="flex flex-col gap-2">
      {grouped.map((g, i) => (
        <div key={g.phase.id || i} className="flex flex-col gap-1">
          {/* Deliberately NOT numbered. The approval card numbers the phases it
              COVERS (1..N), while a plan numbers everything it declares (1..M),
              and a per-phase card covers a subset — so matching chips would
              assert a correspondence that does not hold and point a reader at
              the wrong phase. The agent's own label is the heading; the card
              keeps the numbers, because only it knows what it is clearing. */}
          <div className="break-words text-xs font-medium text-card-foreground">{g.phase.label}</div>
          <div className="border-l border-border/60 pl-2">
            <StepList items={g.steps} live={live} />
          </div>
        </div>
      ))}
      {ungrouped.length > 0 && (
        <div className="flex flex-col gap-1">
          {grouped.length > 0 && (
            <span className="text-[10px] font-medium uppercase tracking-wide text-muted-foreground">Other steps</span>
          )}
          <div className={cn(grouped.length > 0 && "border-l border-border/60 pl-2")}>
            <StepList items={ungrouped} live={live} />
          </div>
        </div>
      )}
    </div>
  );
}

// PlanBlock renders a plan snapshot as a checklist card — the plan's steps,
// grouped under their declared phases, with per-status glyphs. Handles a
// missing/empty item list gracefully (renders just the header).
function PlanBlock({ plan, live }: { plan: PlanUpdateInner; live: boolean }) {
  const items: PlanItemRef[] = plan.items ?? [];
  return (
    <div className="mx-auto w-full max-w-[85%] rounded-lg border border-border bg-card/60 px-3 py-2">
      <div className="flex items-center gap-1.5 text-xs font-medium text-muted-foreground mb-1.5">
        <ListChecks className="h-3.5 w-3.5" aria-hidden="true" />
        <span>Plan{items.length > 0 ? ` · ${items.length} step${items.length === 1 ? "" : "s"}` : ""}</span>
      </div>
      {items.length === 0 ? (
        <div className="text-xs text-muted-foreground">Plan updated.</div>
      ) : (
        <PlanSteps plan={plan} live={live} />
      )}
    </div>
  );
}

// Bubble is the shared agent/user speech-bubble shell. isUser aligns right in
// the primary color; otherwise left on a card surface. className layers extra
// per-state styling (e.g. a pending send's faded pulse) onto the inner bubble.
function Bubble({ isUser, className, children }: { isUser: boolean; className?: string; children: React.ReactNode }) {
  return (
    <div className={cn("flex", isUser ? "justify-end" : "justify-start")}>
      <div
        className={cn(
          "max-w-[75%] rounded-2xl px-3.5 py-2 text-sm leading-relaxed",
          isUser
            ? "bg-primary text-primary-foreground rounded-br-sm"
            : "bg-card border border-border text-card-foreground rounded-bl-sm",
          className,
        )}
      >
        {children}
      </div>
    </div>
  );
}

// Line renders one committed timeline entry by role: user/agent bubbles,
// system/error centered notes, plan checklists, and interaction cards.
// onRetry re-sends a failed user line (its bubble carries the Retry
// affordance); onDecision fires a decision-kind InteractionCard action.
//
// Memoized: `line` objects are stable by id (a committed bubble's object is
// never re-created after append), and onRetry/onDecision/live are stable
// within a turn, so the whole list does NOT re-render (and MessageText does
// not re-parse markdown) when a stream_delta bumps only the live streaming
// draft.
const Line = memo(function Line({
  line,
  toolSession,
  onRetry,
  onDecision,
  live,
  absorbedPlan,
}: {
  line: ChatLine;
  // absorbedPlan is the plan snapshot this line's card folds in, set only for a
  // plan-gate interaction. The list computes it (it needs the whole timeline to
  // find the plan a card covers); the row just renders it.
  absorbedPlan?: PlanUpdateInner;
  // toolSession is THIS line's transcript, not the whole map: passing the map
  // would hand every memoized row a new object on every output chunk and
  // re-render the entire timeline. Non-tool-session rows get a stable
  // undefined, so only the streaming block actually re-renders.
  toolSession?: ToolSessionState;
  onRetry?: (id: string, text: string, requestId: string) => void;
  onDecision?: (requestRef: string, category: string, actionId: string) => void;
  live: boolean;
}) {
  if (line.role === "toolSession") {
    // The line only POSITIONS the block; its content arrives separately. A ref
    // with no state yet renders nothing rather than an empty shell.
    return toolSession ? <ToolSessionBlock state={toolSession} /> : null;
  }
  if (line.liveViewUrl) {
    // The agent's artifact_offer_view: a centered "View live" link opening the
    // artifact viewer in a new tab.
    return (
      <div className="flex justify-center py-1">
        <a
          href={line.liveViewUrl}
          target="_blank"
          rel="noopener noreferrer"
          className="inline-flex items-center gap-1.5 rounded-lg border border-border bg-card px-3 py-1.5 text-xs font-medium text-card-foreground hover:bg-accent hover:text-accent-foreground"
        >
          <ExternalLink className="h-3.5 w-3.5 shrink-0" aria-hidden="true" />
          View live
        </a>
      </div>
    );
  }
  if (line.role === "plan" && line.plan) {
    return <PlanBlock plan={line.plan} live={live} />;
  }
  if (line.role === "interaction" && line.interactionRequest) {
    return (
      <InteractionCard
        request={line.interactionRequest}
        applied={line.interactionApplied}
        onDecision={onDecision ?? (() => {})}
        // A plan-gate card folds in the plan it is clearing, so the approval
        // and the work it authorizes are one card rather than two describing
        // the same thing in different vocabularies. Absent for every other
        // category, and absent when no plan snapshot has arrived yet.
        footer={
          absorbedPlan ? (
            <div className="flex flex-col gap-1 border-t border-border/60 pt-2">
              <span className="text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
                The plan this covers
              </span>
              <PlanSteps plan={absorbedPlan} live={live} />
            </div>
          ) : undefined
        }
      />
    );
  }
  if (line.role === "notice" && line.notice) {
    return <NoticeCard notice={line.notice} />;
  }
  if (line.role === "opening" && line.opening) {
    return <OpeningCard opening={line.opening} />;
  }
  if (line.role === "system" || line.role === "error") {
    return (
      <div
        className={cn(
          "mx-auto max-w-[85%] text-center text-xs py-1",
          line.role === "error" ? "text-destructive" : "text-muted-foreground",
        )}
      >
        <PlainText text={line.text} />
      </div>
    );
  }
  if (line.role === "user" && line.sendStatus === "failed") {
    // The send errored/timed out: mark THIS bubble red with the reason inline
    // and a Retry, rather than a separate bottom error line — the failure stays
    // attached to the message that failed.
    return (
      <div className="flex flex-col items-end gap-1" data-testid="failed-message">
        <Bubble isUser className="border border-destructive bg-destructive/10 text-foreground">
          <MessageText text={line.text} />
        </Bubble>
        <div className="flex items-center gap-2 text-[11px] text-destructive">
          <AlertCircle className="h-3.5 w-3.5 shrink-0" aria-hidden="true" />
          <span className="break-words">{line.sendError || "Failed to send."}</span>
          {onRetry && (
            <button
              type="button"
              onClick={() => onRetry(line.id, line.text, line.requestId ?? "")}
              className="inline-flex items-center gap-1 font-medium underline underline-offset-2 hover:no-underline"
            >
              <RotateCw className="h-3 w-3" aria-hidden="true" />
              Retry
            </button>
          )}
        </div>
      </div>
    );
  }
  if (line.role === "user" && line.sendStatus === "pending") {
    // Optimistic, not yet acked by channelsd: the bubble is up instantly (the
    // composer never waits on the round-trip) but faded + pulsing so it clearly
    // reads as in-flight until it resolves to sent or failed.
    return (
      <div data-testid="pending-message">
        <Bubble isUser className="opacity-60 animate-pulse">
          <MessageText text={line.text} />
        </Bubble>
      </div>
    );
  }
  if (line.role === "user" && line.queued) {
    // Sent while the agent was already working the current turn: it POSTed
    // immediately (the P1a backend holds it server-side), but this is the
    // SAME bubble as a normal user line — grayed with a "Queued" label until
    // turn_activity(active:false) clears the flag in place (see ChatView's
    // handleFrame), un-graying it to a normal Bubble. One render, not a
    // separate duplicate.
    return (
      <div data-testid="queued-message">
        <Bubble isUser>
          <div className="mb-0.5 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">Queued</div>
          <span className="whitespace-pre-wrap break-words text-muted-foreground">{line.text}</span>
        </Bubble>
      </div>
    );
  }
  if (line.attachments && line.attachments.length > 0) {
    // An agent reply that carried attached artifacts: the text bubble plus a row
    // of download chips beneath it, aligned with the speaker.
    return (
      <div className={cn("flex flex-col gap-1", line.role === "user" ? "items-end" : "items-start")}>
        <Bubble isUser={line.role === "user"}>
          <MessageText text={line.text} />
        </Bubble>
        <AttachmentChips attachments={line.attachments} />
      </div>
    );
  }
  return (
    <Bubble isUser={line.role === "user"}>
      <MessageText text={line.text} />
    </Bubble>
  );
});

// AttachmentChips renders each attached artifact as a clickable download link.
// The href is a same-origin /artifact-download URL that responds with
// Content-Disposition: attachment, so the browser downloads rather than renders.
function AttachmentChips({ attachments }: { attachments: NonNullable<ChatLine["attachments"]> }) {
  return (
    <div className="flex flex-wrap gap-1.5" data-testid="attachment-chips">
      {attachments.map((a, i) => (
        <a
          key={i}
          href={a.url}
          download={a.filename}
          className="inline-flex items-center gap-1.5 rounded-lg border border-border bg-card px-2.5 py-1 text-xs text-card-foreground hover:bg-accent hover:text-accent-foreground"
        >
          <Download className="h-3.5 w-3.5 shrink-0" aria-hidden="true" />
          <span className="break-all">{a.filename}</span>
        </a>
      ))}
    </div>
  );
}

// StreamingBubble is the in-progress assistant reply: the accumulated token
// deltas plus a blinking block cursor, rendered live before the final
// user_message finalizes it.
function StreamingBubble({ text }: { text: string }) {
  return (
    <Bubble isUser={false}>
      <PlainText text={text} />
      <span
        className="ml-0.5 inline-block h-4 w-[2px] translate-y-0.5 bg-current animate-pulse"
        aria-hidden="true"
      />
    </Bubble>
  );
}

// WorkingIndicator is the "agent is working" affordance shown between a send
// and the first streamed token (or, when streaming is off, for the whole
// turn): three bouncing dots. It vanishes the instant the turn completes
// (working goes false). The update_status caption is NOT rendered here — it
// lives on its own CaptionLine so it survives the streaming bubble replacing
// this throbber (the bug where the caption vanished the moment tokens streamed).
function WorkingIndicator() {
  return (
    <div className="flex justify-start" role="status" aria-label="Agent is working">
      <div className="flex items-center gap-2 rounded-2xl rounded-bl-sm border border-border bg-card px-3.5 py-2.5">
        <div className="flex items-center gap-1">
          {[0, 150, 300].map((delay) => (
            <span
              key={delay}
              className="h-1.5 w-1.5 rounded-full bg-muted-foreground animate-bounce"
              style={{ animationDelay: `${delay}ms` }}
            />
          ))}
        </div>
      </div>
    </div>
  );
}

// CaptionLine is the agent's live update_status caption ("Reading files…"),
// mirroring Slack's persistent setStatus. It renders on its own line whenever a
// turn is active and a caption is present — crucially NOT gated on the streaming
// state — so it stays visible while the agent streams its reply, unlike the
// throbber (which the streaming bubble replaces).
function CaptionLine({ text }: { text: string }) {
  return (
    <div className="flex items-center justify-center text-[11px] text-muted-foreground py-0.5">
      <span className="animate-pulse">{text}</span>
    </div>
  );
}

// StatusLine is the subtle "working · 34s · 1.2K in · 6.4K out" line driven by
// turn-progress frames, shown while the agent is active.
function StatusLine({ text }: { text: string }) {
  return (
    <div className="flex items-center justify-center gap-1.5 text-[11px] text-muted-foreground py-0.5">
      <Loader2 className="h-3 w-3 animate-spin" aria-hidden="true" />
      <span>{text}</span>
    </div>
  );
}

// MessageList renders the conversation timeline plus the live region (streaming
// bubble / working throbber / status line) and keeps the view pinned to the
// latest content as it grows.
export function MessageList({
  lines,
  streamingText,
  working,
  statusText,
  notice,
  opTree,
  toolSessions,
  onRetry,
  onDecision,
}: {
  lines: ChatLine[];
  streamingText: string;
  working: boolean;
  statusText: string | null;
  notice: string;
  opTree: OperationActivityNode[];
  toolSessions: ToolSessionMap;
  onRetry?: (id: string, text: string, requestId: string) => void;
  onDecision?: (requestRef: string, category: string, actionId: string) => void;
}) {
  const endRef = useRef<HTMLDivElement>(null);
  const scrollRef = useRef<HTMLDivElement>(null);
  const rafRef = useRef<number | null>(null);

  useEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    // Only auto-scroll when the reader is already near the bottom — a manual
    // scroll up to re-read history must NOT be yanked back down on every token.
    // (In jsdom all three metrics are 0, so this reads as "near bottom" — the
    // effect stays a no-op there because scrollIntoView is also stubbed out.)
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 120;
    if (!nearBottom) return;
    // Throttle to one scroll per animation frame so a burst of stream_delta
    // ticks (each bumping streamingText) coalesces into a single scrollIntoView
    // instead of thrashing layout per token. rAF is guarded for jsdom, which
    // (like scrollIntoView) may not implement it.
    if (typeof requestAnimationFrame !== "function") {
      endRef.current?.scrollIntoView?.({ block: "end" });
      return;
    }
    if (rafRef.current != null) cancelAnimationFrame(rafRef.current);
    rafRef.current = requestAnimationFrame(() => {
      rafRef.current = null;
      endRef.current?.scrollIntoView?.({ block: "end" });
    });
    return () => {
      if (rafRef.current != null) {
        cancelAnimationFrame(rafRef.current);
        rafRef.current = null;
      }
    };
  }, [lines.length, streamingText, working, statusText, notice, opTree]);

  // Fold each plan-gate card together with the plan it clears: the card takes
  // the most recent plan snapshot before it, and that snapshot's own line is
  // suppressed so the pair renders as ONE card. Without this the timeline shows
  // a flat checklist and an approval speaking in phases side by side — the same
  // work described twice, in two vocabularies, which is what made the approval
  // hard to read in the first place.
  //
  // Keyed off the LINE ORDER rather than any id the payloads share, because
  // they share none: a plan update and an approval request are independent
  // envelopes. "The plan in force when this card was published" is exactly the
  // most recent one before it.
  const { absorbedPlanByLine, suppressedPlanLines } = useMemo(() => {
    const byLine: Record<string, PlanUpdateInner> = {};
    const suppressed = new Set<string>();
    let lastPlan: ChatLine | undefined;
    for (const l of lines) {
      if (l.role === "plan" && l.plan) {
        lastPlan = l;
        continue;
      }
      const category = l.interactionRequest?.category;
      if (l.role === "interaction" && (category === "plan_phase" || category === "plan_amendment")) {
        if (lastPlan?.plan) {
          byLine[l.id] = lastPlan.plan;
          suppressed.add(lastPlan.id);
        }
      }
    }
    return { absorbedPlanByLine: byLine, suppressedPlanLines: suppressed };
  }, [lines]);

  const showStreaming = streamingText.length > 0;
  // The throbber is the "working but nothing to show yet" affordance — once
  // tokens stream, the streaming bubble's own cursor carries the liveness, so
  // don't stack a throbber under it.
  const showThrobber = working && !showStreaming;

  return (
    <div ref={scrollRef} className="flex-1 min-h-0 overflow-y-auto px-4 py-4">
      <div className="flex flex-col gap-2.5 max-w-3xl mx-auto">
        {lines.map((l) =>
          suppressedPlanLines.has(l.id) ? null : (
            <div key={l.id} data-testid="chat-line" data-role={l.role}>
              <Line
                line={l}
                toolSession={l.toolSessionRef ? toolSessions[l.toolSessionRef] : undefined}
                onRetry={onRetry}
                onDecision={onDecision}
                live={working}
                absorbedPlan={absorbedPlanByLine[l.id]}
              />
            </div>
          ),
        )}
        {showStreaming && <StreamingBubble text={streamingText} />}
        {showThrobber && <WorkingIndicator />}
        {working && notice && <CaptionLine text={notice} />}
        {working && opTree.length > 0 && <OperationTree nodes={opTree} />}
        {working && statusText && <StatusLine text={statusText} />}
        <div ref={endRef} />
      </div>
    </div>
  );
}
