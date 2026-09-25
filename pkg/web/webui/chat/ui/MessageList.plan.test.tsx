import * as React from "react";
import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, within } from "@testing-library/react";
import { MessageList } from "./MessageList";
import type { ChatLine, InteractionRequestInner, PlanUpdateInner } from "./types";

afterEach(cleanup);

const emptyList = {
  streamingText: "",
  working: false,
  statusText: null,
  notice: "",
  opTree: [],
  toolSessions: {},
};

const phasedPlan: PlanUpdateInner = {
  planName: "main",
  updatedAt: "2026-08-16T00:00:00Z",
  phases: [
    { id: "recon", label: "Read the repository" },
    { id: "write", label: "Push the fix" },
  ],
  items: [
    { id: "s1", label: "clone the repo", status: "done", phase: "recon" },
    { id: "s2", label: "read the failing test", status: "done", phase: "recon" },
    { id: "s3", label: "push the branch", status: "pending", phase: "write" },
    { id: "s4", label: "tidy up afterwards", status: "pending" },
  ],
};

const planLine: ChatLine = { id: "line-plan", role: "plan", text: "", plan: phasedPlan };

function approvalRequest(category: string): InteractionRequestInner {
  return {
    agentSessionRef: { namespace: "default", name: "sess-1" },
    category,
    requestRef: "req-plan-1",
    lead: "Approve this plan",
    audience: { scope: "requester" },
    fields: [{ label: "What", value: "", items: [{ text: "Phase 1", items: [{ text: "push to a repository" }] }] }],
    actions: [{ id: "approve", label: "Approve", kind: "decision", style: "primary" }],
  };
}

describe("plan rendering", () => {
  it("groups steps under the phases the agent declared, in order", () => {
    render(<MessageList lines={[planLine]} {...emptyList} />);

    // Before this, the plan card was a flat checklist and named no phase at all.
    expect(screen.getByText("Read the repository")).toBeTruthy();
    expect(screen.getByText("Push the fix")).toBeTruthy();
  });

  it("does NOT number the plan's phases, so they cannot be misread as the card's", () => {
    // The approval card numbers the phases it COVERS; a plan numbers everything
    // it DECLARES, and a per-phase card covers a subset. Matching "Phase N"
    // chips on both sides would point a reader at the wrong phase.
    render(<MessageList lines={[planLine]} {...emptyList} />);

    expect(screen.queryByText("Phase 1")).toBeNull();
    expect(screen.queryByText("Phase 2")).toBeNull();
  });

  it("keeps a step whose phase matches nothing rather than dropping it", () => {
    // `phase` is a free-text display hint the agent rewrites on every
    // update_plan call, so a stale or misspelled id must cost the step its
    // heading, never its presence.
    const stale: PlanUpdateInner = {
      ...phasedPlan,
      items: [
        { id: "s1", label: "clone the repo", status: "done", phase: "recon" },
        { id: "s9", label: "orphaned step", status: "pending", phase: "no-such-phase" },
      ],
    };
    render(<MessageList lines={[{ ...planLine, plan: stale }]} {...emptyList} />);

    expect(screen.getByText("orphaned step")).toBeTruthy();
    expect(screen.getByText("Other steps")).toBeTruthy();
  });

  it("falls back to a flat list when the plan declares no phases", () => {
    const flat: PlanUpdateInner = {
      planName: "main",
      updatedAt: "2026-08-16T00:00:00Z",
      items: [{ id: "s1", label: "just do it", status: "pending" }],
    };
    render(<MessageList lines={[{ ...planLine, plan: flat }]} {...emptyList} />);

    expect(screen.getByText("just do it")).toBeTruthy();
    expect(screen.queryByText("Phase 1")).toBeNull();
    expect(screen.queryByText("Other steps")).toBeNull();
  });
});

describe("plan-gate approval folds in the plan it covers", () => {
  const planGateCategories = ["plan_phase", "plan_amendment"];

  planGateCategories.forEach((category) => {
    it(`${category}: renders ONE card carrying both the approval and the plan's steps`, () => {
      const lines: ChatLine[] = [
        planLine,
        { id: "line-card", role: "interaction", text: "", interactionRequest: approvalRequest(category) },
      ];
      render(<MessageList lines={lines} {...emptyList} onDecision={vi.fn()} />);

      // Exactly one card, and the plan's own line is gone — two cards
      // describing the same work in different vocabularies was the defect.
      const cards = screen.getAllByTestId("interaction-card");
      expect(cards.length).toBe(1);
      expect(screen.queryByText(/^Plan · /)).toBeNull();

      // The steps live INSIDE the approval card, not beside it.
      const card = cards[0];
      expect(within(card).getByText("The plan this covers")).toBeTruthy();
      expect(within(card).getByText("clone the repo")).toBeTruthy();
      expect(within(card).getByText("push the branch")).toBeTruthy();
      // ...still grouped, and still keeping the ungrouped step.
      expect(within(card).getByText("Read the repository")).toBeTruthy();
      expect(within(card).getByText("tidy up afterwards")).toBeTruthy();
      // ...alongside the approval's own authority lines.
      expect(within(card).getByText("push to a repository")).toBeTruthy();
    });
  });

  it("leaves a non-plan-gate card alone, and leaves the plan card standing", () => {
    const lines: ChatLine[] = [
      planLine,
      { id: "line-card", role: "interaction", text: "", interactionRequest: approvalRequest("tool_approval") },
    ];
    render(<MessageList lines={lines} {...emptyList} onDecision={vi.fn()} />);

    // A tool approval says nothing about the plan, so folding one into the
    // other would assert a relationship that does not exist.
    expect(screen.getByText(/^Plan · /)).toBeTruthy();
    expect(screen.queryByText("The plan this covers")).toBeNull();
  });

  it("renders the card alone when no plan snapshot preceded it", () => {
    const lines: ChatLine[] = [
      { id: "line-card", role: "interaction", text: "", interactionRequest: approvalRequest("plan_phase") },
    ];
    render(<MessageList lines={lines} {...emptyList} onDecision={vi.fn()} />);

    expect(screen.getAllByTestId("interaction-card").length).toBe(1);
    expect(screen.queryByText("The plan this covers")).toBeNull();
  });
});
