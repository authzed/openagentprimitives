import "@testing-library/jest-dom/vitest";
import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { GoalSessionAlerts } from "./GoalSessionAlerts";
import type { SessionRow } from "./types";

beforeEach(() => sessionStorage.clear());
afterEach(cleanup);
const row: SessionRow = { ns: "default", name: "goal-run", uid: "first", class: "reminders", title: "Reminders", phase: "Starting", awaitingHuman: false, ended: false, goalCreated: true, openingSummary: "Session created for goal Stretch <img src=x>" };

describe("goal session alerts", () => {
  it("alerts on creation before an approval is pending and persists until dismissed", () => {
    const notify = vi.fn();
    const props = { sessions: [row], subject: "owner", notify };
    const { container, unmount, rerender } = render(<GoalSessionAlerts {...props} />);
    expect(screen.getByRole("alert")).toHaveTextContent(row.openingSummary!);
    expect(container.querySelector("img")).toBeNull();
    expect(screen.getByRole("link", { name: "Open session" })).toHaveAttribute("href", "/sessions?session=default%2Fgoal-run");
    expect(notify).toHaveBeenCalledTimes(1);
    expect(notify.mock.calls[0].join(" ")).not.toContain("Stretch");
    rerender(<GoalSessionAlerts {...props} sessions={[{ ...row, awaitingHuman: true }]} />);
    expect(screen.getByRole("link", { name: "Open session to review approval" })).toBeInTheDocument();
    expect(notify).toHaveBeenCalledTimes(1);
    unmount();
    const next = render(<GoalSessionAlerts {...props} />);
    expect(screen.getByRole("alert")).toBeInTheDocument();
    expect(notify).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: "Dismiss alert for Reminders" }));
    expect(screen.queryByRole("alert")).toBeNull();
    next.unmount();
    render(<GoalSessionAlerts {...props} />);
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("acknowledges a session when opened and does not alert again when leaving it", () => {
    const props = { sessions: [row], subject: "owner", notify: vi.fn() };
    const { rerender, unmount } = render(<GoalSessionAlerts {...props} />);
    expect(screen.getByRole("alert")).toBeInTheDocument();
    rerender(<GoalSessionAlerts {...props} selected={{ ns: row.ns, name: row.name }} />);
    expect(screen.queryByRole("alert")).toBeNull();
    rerender(<GoalSessionAlerts {...props} />);
    expect(screen.queryByRole("alert")).toBeNull();
    unmount();
    render(<GoalSessionAlerts {...props} />);
    expect(screen.queryByRole("alert")).toBeNull();
    expect(props.notify).toHaveBeenCalledTimes(1);
  });

  it("ignores ordinary and ended sessions but recognizes a replacement with a new UID", () => {
    const props = { subject: "owner", notify: vi.fn() };
    const { rerender } = render(<GoalSessionAlerts {...props} sessions={[{ ...row, goalCreated: false }, { ...row, name: "old", ended: true }]} />);
    expect(screen.queryByRole("alert")).toBeNull();
    rerender(<GoalSessionAlerts {...props} sessions={[row]} />);
    fireEvent.click(screen.getByRole("button", { name: "Dismiss alert for Reminders" }));
    rerender(<GoalSessionAlerts {...props} sessions={[{ ...row, uid: "replacement" }]} />);
    expect(screen.getByRole("alert")).toBeInTheDocument();
    expect(props.notify).toHaveBeenCalledTimes(2);
  });

  it("removes alerts when access disappears and isolates acknowledgement by signed-in user", () => {
    const props = { subject: "owner", notify: vi.fn() };
    const { rerender, unmount } = render(<GoalSessionAlerts {...props} sessions={[row]} />);
    rerender(<GoalSessionAlerts {...props} sessions={[]} />);
    expect(screen.queryByRole("alert")).toBeNull();
    rerender(<GoalSessionAlerts {...props} sessions={[row]} />);
    fireEvent.click(screen.getByRole("button", { name: "Dismiss alert for Reminders" }));
    unmount();
    render(<GoalSessionAlerts {...props} subject="another-user" sessions={[row]} />);
    expect(screen.getByRole("alert")).toBeInTheDocument();
  });
});
