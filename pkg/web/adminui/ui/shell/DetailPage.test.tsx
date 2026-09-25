import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { cleanup, render, screen, fireEvent } from "@testing-library/react";
import { DetailPage } from "./DetailPage";

afterEach(() => cleanup());
beforeEach(() => window.history.pushState({}, "", "/admin/agent/default/x"));

describe("DetailPage", () => {
  const tabs = [
    { id: "overview", label: "Overview" },
    { id: "activity", label: "Activity" },
  ];

  it("back button navigates to backRoute when set (not history.back)", () => {
    render(
      <DetailPage
        title="x"
        backRoute={{ type: "view", view: "agents" }}
        tabs={tabs}
        activeTab="overview"
        onTab={() => {}}
      >
        <p>body</p>
      </DetailPage>,
    );
    fireEvent.click(screen.getByText("Back"));
    expect(window.location.pathname).toBe("/admin/agents");
  });

  it("renders a panel for every tab so no trigger has a dangling aria-controls", () => {
    render(
      <DetailPage title="x" tabs={tabs} activeTab="overview" onTab={() => {}}>
        <p>body</p>
      </DetailPage>,
    );
    const triggers = screen.getAllByRole("tab");
    // Radix wires each trigger's aria-controls to a panel that must exist.
    for (const t of triggers) {
      const panelId = t.getAttribute("aria-controls");
      expect(panelId).toBeTruthy();
      expect(document.getElementById(panelId as string)).not.toBeNull();
    }
  });
});
