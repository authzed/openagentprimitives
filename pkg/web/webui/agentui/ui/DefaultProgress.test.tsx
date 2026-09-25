import "@testing-library/jest-dom/vitest";
import { describe, it, expect, afterEach } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import { DefaultProgress, progressNowLine } from "./DefaultProgress";
import { INITIAL_SESSION_SIGNALS, type SessionSignals } from "../../chat/ui/sessionSignals";

afterEach(cleanup);

// withSignals builds one SessionSignals from the REAL initial constant plus the
// fields the row is about. No row here re-spells the whole shape, so a field
// added to the fold cannot silently default to something this file invented.
function withSignals(over: Partial<SessionSignals>): SessionSignals {
  return { ...INITIAL_SESSION_SIGNALS, ...over };
}

// A plan carrying one step of every bucket, plus one status nothing here
// special-cases — the schema may grow statuses this region has no rule for, and
// dropping such a step would leave the plan reading shorter than it is.
const fourStepPlan = {
  planName: "Ship the change",
  items: [
    { id: "s1", label: "Read the repository", status: "done" },
    { id: "s2", label: "Draft the change", status: "in_progress" },
    { id: "s3", label: "Open a review", status: "pending" },
    { id: "s4", label: "Celebrate", status: "some-future-status" },
  ],
  updatedAt: "2026-09-12T00:00:00Z",
};

// progressNowLine is the precedence itself, separated from the DOM so the
// ordering can be stated once rather than re-derived through six renders.
describe("progressNowLine — what the region says it is doing right now", () => {
  it("says nothing for an idle session", () => {
    expect(progressNowLine(INITIAL_SESSION_SIGNALS)).toBeNull();
  });

  it('falls back to "Working…" when a turn is running and nothing more specific arrived', () => {
    expect(progressNowLine(withSignals({ turnActive: true }))).toBe("Working…");
  });

  it("prefers the live operation line over the generic fallback", () => {
    expect(progressNowLine(withSignals({ turnActive: true, activity: { compactLine: "Connecting GitHub" } }))).toBe(
      "Connecting GitHub",
    );
  });

  it("prefers a mid-turn notice over the generic fallback, and yields to the operation line", () => {
    expect(progressNowLine(withSignals({ turnActive: true, notice: "Waiting on the sandbox" }))).toBe(
      "Waiting on the sandbox",
    );
    expect(
      progressNowLine(
        withSignals({ turnActive: true, notice: "Waiting on the sandbox", activity: { compactLine: "Connecting GitHub" } }),
      ),
    ).toBe("Connecting GitHub");
  });

  // The reducer clears `notice` only at the NEXT turn start and `activity`
  // only on a `cleared` tick, so both routinely outlive the turn that produced
  // them. A now line is a statement that something is happening RIGHT NOW, so
  // a finished turn narrates nothing rather than repeating the last thing it
  // said — which for an ended session would be a live-sounding claim about a
  // session nobody is working on.
  it("says nothing once the turn is over, however much the reducer is still holding", () => {
    expect(progressNowLine(withSignals({ notice: "Waiting on the sandbox" }))).toBeNull();
    expect(progressNowLine(withSignals({ activity: { compactLine: "Connecting GitHub" } }))).toBeNull();
    expect(progressNowLine(withSignals({ ended: true, notice: "Waiting on the sandbox" }))).toBeNull();
  });

  it("prefers the composing line over everything else — the page itself is about to change", () => {
    expect(
      progressNowLine(
        withSignals({
          turnActive: true,
          composingUpdate: true,
          notice: "Waiting on the sandbox",
          activity: { compactLine: "Connecting GitHub" },
        }),
      ),
    ).toBe("Composing a page update…");
  });
});

describe("DefaultProgress — the region a page with no ap:progress falls back to", () => {
  it("renders nothing at all for an idle session with no plan", () => {
    render(<DefaultProgress signals={INITIAL_SESSION_SIGNALS} />);
    expect(screen.queryByTestId("agent-ui-default-progress")).toBeNull();
  });

  it("renders the region for an idle session that still has a plan to show", () => {
    render(<DefaultProgress signals={withSignals({ plan: fourStepPlan })} />);
    expect(screen.getByTestId("agent-ui-default-progress")).toBeInTheDocument();
    // Nothing is running, so there is no "now" to state — the plan alone is
    // the content, and an empty line would read as a claim that something is
    // happening.
    expect(screen.queryByTestId("agent-ui-progress-now")).toBeNull();
  });

  it("announces itself politely, so a running turn reaches a screen reader without interrupting", () => {
    render(<DefaultProgress signals={withSignals({ turnActive: true })} />);
    expect(screen.getByTestId("agent-ui-default-progress")).toHaveAttribute("aria-live", "polite");
  });

  it('shows "Working…" while a turn runs and nothing more specific has arrived', () => {
    render(<DefaultProgress signals={withSignals({ turnActive: true })} />);
    expect(screen.getByTestId("agent-ui-progress-now")).toHaveTextContent("Working…");
  });

  it("shows the live operation line in place of the generic one", () => {
    render(<DefaultProgress signals={withSignals({ turnActive: true, activity: { compactLine: "Connecting GitHub" } })} />);
    expect(screen.getByTestId("agent-ui-progress-now")).toHaveTextContent("Connecting GitHub");
  });

  it("shows a mid-turn notice when no operation line is running", () => {
    render(<DefaultProgress signals={withSignals({ turnActive: true, notice: "Waiting on the sandbox" })} />);
    expect(screen.getByTestId("agent-ui-progress-now")).toHaveTextContent("Waiting on the sandbox");
  });

  // The composing line is marked in the DOM as well as worded differently: it
  // is the one now-line that says the PAGE is about to change rather than that
  // work is happening somewhere else, and a reader watching a region rewrite
  // itself needs that distinction to be addressable, not merely phrased.
  it("marks the composing line as such, and lets it win over the operation line", () => {
    render(
      <DefaultProgress
        signals={withSignals({ turnActive: true, composingUpdate: true, activity: { compactLine: "Connecting GitHub" } })}
      />,
    );
    const now = screen.getByTestId("agent-ui-progress-now");
    expect(now).toHaveTextContent("Composing a page update…");
    expect(now).toHaveAttribute("data-composing", "true");
  });

  it("does not mark an ordinary now-line as composing", () => {
    render(<DefaultProgress signals={withSignals({ turnActive: true })} />);
    expect(screen.getByTestId("agent-ui-progress-now")).not.toHaveAttribute("data-composing");
  });

  it("buckets every plan step by its status, and files an unrecognized status under what comes next", () => {
    render(<DefaultProgress signals={withSignals({ turnActive: true, plan: fourStepPlan })} />);
    const items = screen.getAllByRole("listitem");
    expect(items).toHaveLength(4);
    expect(items[0]).toHaveAttribute("data-state", "done");
    expect(items[0]).toHaveTextContent("Read the repository");
    expect(items[1]).toHaveAttribute("data-state", "current");
    expect(items[1]).toHaveTextContent("Draft the change");
    expect(items[2]).toHaveAttribute("data-state", "next");
    expect(items[2]).toHaveTextContent("Open a review");
    // Unknown to this region's three buckets, still shown.
    expect(items[3]).toHaveAttribute("data-state", "next");
    expect(items[3]).toHaveTextContent("Celebrate");
  });

  it("narrates nothing for an idle session with a plan, even while holding a notice", () => {
    render(<DefaultProgress signals={withSignals({ plan: fourStepPlan, notice: "Waiting on the sandbox" })} />);
    expect(screen.getByTestId("agent-ui-default-progress")).toBeInTheDocument();
    expect(screen.queryByTestId("agent-ui-progress-now")).toBeNull();
    // The plan is a record, not a claim about now, so it still renders.
    expect(screen.getAllByRole("listitem")).toHaveLength(4);
  });

  // A step can still READ "in_progress" after the turn ends — the agent
  // yielded without marking it done — and a glyph left spinning there claims
  // work is underway on a session nobody is working on.
  it("stops the in-progress glyph spinning once the turn is over", () => {
    render(<DefaultProgress signals={withSignals({ turnActive: true, plan: fourStepPlan })} />);
    expect(screen.getByTestId("agent-ui-default-progress").querySelector(".animate-spin")).not.toBeNull();
    cleanup();
    render(<DefaultProgress signals={withSignals({ ended: true, plan: fourStepPlan })} />);
    expect(screen.getByTestId("agent-ui-default-progress").querySelector(".animate-spin")).toBeNull();
  });

  it("draws no list at all for a plan the server sent with no items", () => {
    render(<DefaultProgress signals={withSignals({ turnActive: true, plan: { ...fourStepPlan, items: null } })} />);
    expect(screen.getByTestId("agent-ui-default-progress")).toBeInTheDocument();
    expect(screen.queryByRole("list")).toBeNull();
  });
});
