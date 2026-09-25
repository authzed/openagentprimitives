import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import { describe, expect, it, afterEach, vi } from "vitest";
import * as React from "react";
import { AppShell } from "./AppShell";
import type { ViewId } from "./nav";

// Stateful wrapper so we can test nav-click view transitions without AdminApp
function TestShell({
  initialView = "overview" as ViewId,
  email = "test@example.com",
  onSignOut,
}: {
  initialView?: ViewId;
  email?: string;
  onSignOut?: () => void;
}) {
  const [view, setView] = React.useState<ViewId>(initialView);
  return (
    <AppShell
      view={view}
      onNavigate={setView}
      currentUser={{ email, role: "platform admin" }}
      onSignOut={onSignOut}
    >
      <div data-testid="view-content">content:{view}</div>
    </AppShell>
  );
}

describe("AppShell", () => {
  afterEach(() => cleanup());

  it("brand is the product name alone, and hovering it runs the mark's relay band", () => {
    const { container } = render(<TestShell />);
    const brand = screen.getByText("Open Agent Primitives");
    expect(brand.textContent).toBe("Open Agent Primitives");
    const mark = container.querySelector("header svg[data-relay]")!;
    expect(mark.getAttribute("data-relay")).toBe("off");
    fireEvent.pointerEnter(brand.parentElement!);
    expect(mark.getAttribute("data-relay")).toBe("on");
    fireEvent.pointerLeave(brand.parentElement!);
    expect(mark.getAttribute("data-relay")).toBe("off");
  });

  it("renders all 3 explicit group headers (Live, Config, Audit)", () => {
    render(<TestShell />);
    expect(screen.getByText("Live")).toBeTruthy();
    expect(screen.getByText("Config")).toBeTruthy();
    expect(screen.getByText("Audit")).toBeTruthy();
  });

  it("renders all 19 nav item labels", () => {
    render(<TestShell />);
    const labels = [
      // overview group (no header)
      "Overview",
      // Live
      "Agent sessions", "Live tool calls", "Approvals",
      // Config
      "Agents", "Tools", "Skills", "Sources", "Channels",
      "Identity", "Users", "Access", "Settings",
      // Audit
      "Logs", "Sessions", "Budget", "Artifacts", "Memory", "Knowledge",
    ];
    for (const label of labels) {
      // getAllByText because "Sessions" / "Overview" appear in both nav and per-view header
      const matches = screen.getAllByText(label);
      expect(matches.length, `expected "${label}" to appear at least once`).toBeGreaterThan(0);
    }
  });

  it("defaults to Overview — title shows in per-view header", () => {
    render(<TestShell initialView="overview" />);
    // getByRole with name to narrow to the specific h1
    const heading = screen.getByRole("heading", { level: 1, name: /^overview$/i });
    expect(heading.textContent).toBe("Overview");
  });

  it("clicking 'Agent sessions' nav button switches to the live sessions view", () => {
    render(<TestShell />);
    // Click the nav button (first button with text "Agent sessions")
    const navButtons = screen.getAllByRole("button", { name: /agent sessions/i });
    fireEvent.click(navButtons[0]);
    // Per-view header should now show "Agent sessions"
    const heading = screen.getByRole("heading", { level: 1, name: /^agent sessions$/i });
    expect(heading.textContent).toBe("Agent sessions");
    // Children reflect new view
    expect(screen.getByTestId("view-content").textContent).toBe("content:sessions");
  });

  it("clicking 'Logs' nav button switches to the audit logs view", () => {
    render(<TestShell />);
    const navButtons = screen.getAllByRole("button", { name: /^logs$/i });
    fireEvent.click(navButtons[0]);
    const heading = screen.getByRole("heading", { level: 1, name: /^logs$/i });
    expect(heading.textContent).toBe("Logs");
    expect(screen.getByTestId("view-content").textContent).toBe("content:logs");
  });

  it("renders current-user email top-right", () => {
    render(<TestShell email="operator@example.com" />);
    expect(screen.getByText("operator@example.com")).toBeTruthy();
  });

  it("does not render a 'platform admin' role label under the user chip", () => {
    // TestShell still passes role="platform admin"; the chip must no longer show
    // it (every user here is a platform admin — the label was pure noise).
    render(<TestShell email="operator@example.com" />);
    expect(screen.getByText("operator@example.com")).toBeTruthy();
    expect(screen.queryByText(/platform admin/i)).toBeNull();
  });

  it("user chip opens a menu; clicking Sign Out invokes onSignOut", () => {
    const onSignOut = vi.fn();
    render(<TestShell onSignOut={onSignOut} />);
    // Menu is closed by default — no Sign Out item yet.
    expect(screen.queryByRole("menuitem", { name: /sign out/i })).toBeNull();
    // Open via the user-menu button.
    fireEvent.click(screen.getByRole("button", { name: /user menu/i }));
    const signOut = screen.getByRole("menuitem", { name: /sign out/i });
    expect(signOut).toBeTruthy();
    fireEvent.click(signOut);
    expect(onSignOut).toHaveBeenCalledTimes(1);
    // Menu closes after the action.
    expect(screen.queryByRole("menuitem", { name: /sign out/i })).toBeNull();
  });
});
