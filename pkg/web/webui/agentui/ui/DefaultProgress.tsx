import { Loader2 } from "lucide-react";
import { cn } from "@ap/design/lib/utils";
import type { PlanItemRef } from "../../chat/ui/types";
import type { SessionSignals } from "../../chat/ui/sessionSignals";

// DefaultProgress is what a page says about a running turn when the agent has
// not said it itself. It YIELDS to ap:progress: the view checks the declared
// tree and renders this only when no ap:progress is anywhere on the page, so an
// agent that composed its own status card is never doubled by a platform one
// saying the same thing in different words.
//
// Everything here is fed by frames the runner already publishes to every
// surface — the operation line, the turn's plan, mid-turn notices — so a page
// that declares nothing at all still shows work happening. No agent
// cooperation is required and none is possible: this component reads signals,
// never the declaration.
//
// The composing line is PAGE-level rather than per-region because the wire
// carries no hook name while an update_view is still streaming: the frame that
// names the region is the one that arrives when the write LANDS, and until then
// the only honest statement is that the page is about to change.

// progressNowLine is the precedence, stated once.
//
// A RUNNING TURN IS THE PRECONDITION, not merely the last fallback. A now line
// says something is happening right now, and the fold routinely still holds
// the words for a turn that is over: it clears `notice` only at the next turn
// start, and `activity` only on a tick that reports `cleared`. Reading either
// once the turn has stopped would narrate a finished — or ended — session with
// the last thing it said while it was working.
//
// Given a running turn, most specific first: the page rewriting itself beats
// work happening elsewhere, a named operation beats a status caption, and any
// of them beats the generic fallback, which exists only so a running turn is
// never completely silent.
export function progressNowLine(signals: SessionSignals): string | null {
  if (!signals.turnActive) return null;
  if (signals.composingUpdate) return "Composing a page update…";
  if (signals.activity?.compactLine) return signals.activity.compactLine;
  if (signals.notice) return signals.notice;
  return "Working…";
}

// StepState is this region's own three-bucket vocabulary — deliberately
// coarser than the transcript's per-status glyph table, because a fallback
// beside an agent's page is a glance, not a console.
type StepState = "done" | "current" | "next";

// stepState buckets one plan item. An unrecognized status files under what
// comes next rather than being dropped: the plan schema may grow statuses this
// region has no rule for, and a plan that renders shorter than it is would be a
// worse answer than one that renders a step as merely not-yet-done.
function stepState(status: string): StepState {
  switch (status) {
    case "done":
      return "done";
    case "in_progress":
      return "current";
    default:
      return "next";
  }
}

// ProgressStep draws one plan step. The glyph is its own aria-hidden element
// outside the label's text node, so a query for the label text matches the
// label and nothing else.
//
// `live` gates the SPIN, and only the spin: a step can still read "in_progress"
// after the turn ends — the agent yielded without marking it done — and a glyph
// left spinning there would claim work is underway on a session nobody is
// working on. Stopped, it holds the same ring and still says which step the
// plan stopped on.
function ProgressStep({ label, state, live }: { label: string; state: StepState; live: boolean }) {
  return (
    <li data-state={state} className="flex items-center gap-2 text-xs">
      {state === "done" && (
        <span aria-hidden="true" className="text-[hsl(var(--success))]">
          ✓
        </span>
      )}
      {state === "current" && <Loader2 className={cn("h-3 w-3 shrink-0 text-primary", live && "animate-spin")} aria-hidden="true" />}
      <span className={cn(state === "done" && "text-muted-foreground line-through", state === "next" && "text-muted-foreground")}>
        {label}
      </span>
    </li>
  );
}

export function DefaultProgress({ signals }: { signals: SessionSignals }): JSX.Element | null {
  // Nothing running and no plan to show is not a quiet region — it is no
  // region at all. A permanently-present empty strip would take space on every
  // page for a session that is simply idle.
  if (!signals.turnActive && signals.plan === null) return null;

  const now = progressNowLine(signals);
  const items: PlanItemRef[] = signals.plan?.items ?? [];

  return (
    <section
      data-testid="agent-ui-default-progress"
      // Polite, never assertive: a turn starting must reach a screen reader
      // without interrupting whatever the viewer is doing on the page below.
      aria-live="polite"
      className="flex flex-col gap-2 border-b border-border bg-card/40 px-4 py-2"
    >
      {now !== null && (
        <p
          data-testid="agent-ui-progress-now"
          // Addressable as well as worded differently: the composing line is
          // the one that says THIS PAGE is about to change, rather than that
          // work is happening somewhere else.
          data-composing={signals.composingUpdate ? "true" : undefined}
          className="flex items-center gap-2 text-xs font-medium text-foreground"
        >
          {/* A now line exists only while the turn runs (see progressNowLine),
              so the spinner needs no liveness check of its own. The composing
              line goes without it: that line says the page is about to redraw
              under the reader, and wearing the same generic spinner as every
              other line would file it as ordinary background work. */}
          {!signals.composingUpdate && (
            <Loader2 className="h-3 w-3 shrink-0 animate-spin text-muted-foreground" aria-hidden="true" />
          )}
          {now}
        </p>
      )}
      {items.length > 0 && (
        <ul className="flex flex-col gap-1">
          {items.map((it, i) => (
            <ProgressStep key={`${i}-${it.id}`} label={it.label} state={stepState(it.status)} live={signals.turnActive} />
          ))}
        </ul>
      )}
    </section>
  );
}
