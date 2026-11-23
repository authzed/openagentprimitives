import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { InteractionCard } from "./InteractionCard";
import type { InteractionRequestInner } from "./types";

afterEach(cleanup);

function requestWithReminders(differentLimits = false): InteractionRequestInner {
  const children: InteractionRequestInner[] = ["Stretch <img src=x>", "Water reminder"].map((title, i) => ({
    agentSessionRef: { namespace: "default", name: "session" },
    category: "goal_execution_consent",
    requestRef: `reminder-${i}`,
    lead: "Allow one private reminder?",
    body: "Runs once; a fresh plan approval is required before delivery.",
    audience: { scope: "requester" },
    fields: [
      { label: "Run once", value: `21:${i ? "15" : "13"} UTC` },
      { label: "Authorization ends", value: `21:${i ? "25" : "23"} UTC` },
      { label: "Limits", value: differentLimits && i ? "300 seconds" : "180 seconds" },
      { label: "Private recipient", value: "alice@example.com" },
    ],
    excerpt: { label: "Goal", content: `${title}\n\nOutcome: private reminder\n\nRequired evidence: delivered reply` },
  }));
  return {
    ...children[0], category: "plan_phase", requestRef: "plan", lead: "Approve this plan and 2 private reminders?",
    body: "Review both reminders before approving.", consents: children, details: children,
    fields: [
      { label: "Plan", value: "Schedule both reminders" },
      ...children.flatMap((child, i) => [
        { label: `Reminder ${i + 1}`, value: child.lead },
        { label: `Reminder ${i + 1} · Execution`, value: child.body! },
        ...child.fields!.map((field) => ({ ...field, label: `Reminder ${i + 1} · ${field.label}` })),
      ]),
    ],
    excerpt: { label: "Reminders included in this approval", content: children.map((c) => c.excerpt!.content).join("\n\n") },
  };
}

describe("combined approval review", () => {
  it("shows shared terms once, preserves distinct times, and retains exact details after refresh", () => {
    const request = requestWithReminders();
    const original = JSON.stringify(request);
    const { container, rerender } = render(<InteractionCard request={request} onDecision={vi.fn()} />);
    const compact = screen.getByTestId("compact-consent-review");
    expect(within(compact).getAllByText("180 seconds")).toHaveLength(1);
    expect(within(compact).getAllByText("alice@example.com")).toHaveLength(1);
    expect(within(compact).getByText("21:13 UTC")).toBeTruthy();
    expect(within(compact).getByText("21:15 UTC")).toBeTruthy();
    expect(within(compact).getByText("21:23 UTC")).toBeTruthy();
    expect(within(compact).getByText("21:25 UTC")).toBeTruthy();
    expect(compact.textContent).toContain("Stretch <img src=x>");
    expect(container.querySelector("img")).toBeNull();
    expect(screen.getByText("Schedule both reminders")).toBeTruthy();
    const disclosures = container.querySelectorAll("details");
    expect(disclosures).toHaveLength(2);
    expect(disclosures[0].hasAttribute("open")).toBe(false);
    expect(disclosures[0].textContent).toContain("Required evidence: delivered reply");
    const exact = disclosures[1].querySelector("pre")!.textContent;
    expect(exact).toBe(JSON.stringify(request.details, null, 2));
    expect(JSON.stringify(request)).toBe(original);
    rerender(<InteractionCard request={JSON.parse(original)} applied={{
      agentSessionRef: request.agentSessionRef, category: request.category, requestRef: request.requestRef, outcome: "approved",
    }} onDecision={vi.fn()} />);
    expect(screen.getByTestId("compact-consent-review").textContent).toBe(compact.textContent);
    expect(container.querySelectorAll("details")[1].querySelector("pre")!.textContent).toBe(exact);
  });

  it("keeps differing limits visible on their respective reminders", () => {
    render(<InteractionCard request={requestWithReminders(true)} onDecision={vi.fn()} />);
    expect(within(screen.getByRole("region", { name: "Reminder 1" })).getByText("180 seconds")).toBeTruthy();
    expect(within(screen.getByRole("region", { name: "Reminder 2" })).getByText("300 seconds")).toBeTruthy();
  });

  it("keeps the original display when the fields are not the recognized complete projection", () => {
    const request = requestWithReminders();
    request.fields![1].value = "Different review text";
    render(<InteractionCard request={request} onDecision={vi.fn()} />);
    expect(screen.queryByTestId("compact-consent-review")).toBeNull();
    expect(screen.getByText("Different review text")).toBeTruthy();
    expect(screen.getByText("View exact requests included in this approval")).toBeTruthy();
  });
});
