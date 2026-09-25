import { render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import * as React from "react";
import { ToolCallsView } from "./ToolCallsView";

afterEach(() => vi.restoreAllMocks());

describe("ToolCallsView", () => {
  it("renders newest-first tool calls with their sessions and agents", async () => {
    vi.stubGlobal("fetch", vi.fn(async () =>
      new Response(JSON.stringify([
        { t: "2026-06-30T12:00:05Z", namespace: "default", name: "sess-a", agentClass: "deploy-agent", tool: "code_gh" },
        { t: "2026-06-30T12:00:01Z", namespace: "team", name: "sess-b", agentClass: "support-bot", tool: "linear_list_issues" },
      ]), { status: 200 })));

    render(<ToolCallsView apiBase="/admin/api" />);

    await waitFor(() => expect(screen.getByText("code_gh")).toBeTruthy());
    expect(screen.getByText("linear_list_issues")).toBeTruthy();

    // Session + agent cells are EntityLinks deep-linking to their detail pages.
    // The default namespace is hidden in the label (link name = bare "sess-a"),
    // but the route id keeps the full "ns/name". A non-default namespace (team)
    // still shows in the label.
    const sessLink = screen.getByRole("link", { name: "sess-a" });
    expect(sessLink.getAttribute("href")).toBe("/admin/session/default/sess-a");
    // Non-default namespace stays in the label; assert via the name portion.
    const teamLink = screen.getByText("sess-b").closest("a") as HTMLAnchorElement;
    expect(teamLink.getAttribute("href")).toBe("/admin/session/team/sess-b");
    // The AgentClass is namespaced under the session's namespace.
    const agentLink = screen.getByRole("link", { name: "deploy-agent" });
    expect(agentLink.getAttribute("href")).toBe("/admin/agent/default/deploy-agent");
    // Tool stays plain text (no CR resolution) — not a link.
    expect(screen.queryByRole("link", { name: "code_gh" })).toBeNull();
  });

  it("shows an empty state when there are no tool calls", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("[]", { status: 200 })));
    render(<ToolCallsView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText(/No tool calls yet/)).toBeTruthy());
  });

  it("surfaces a server error in a banner", async () => {
    vi.stubGlobal("fetch", vi.fn(async () =>
      new Response(JSON.stringify({ error: "boom" }), { status: 500 })));
    render(<ToolCallsView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText(/Could not load tool calls: boom/)).toBeTruthy());
  });
});
