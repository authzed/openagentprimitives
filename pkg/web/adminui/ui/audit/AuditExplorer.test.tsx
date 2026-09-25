import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import * as React from "react";
import { AuditExplorer } from "./AuditExplorer";
import type { AuditEvent } from "../lib/api";

afterEach(cleanup);

const ev1: AuditEvent = {
  time: "2026-06-01T12:00:00Z", sessionNamespace: "default", sessionName: "s1",
  agentClass: "deploy-agent", kind: "approval", actor: "user:abc", tool: "terraform_apply",
  outcome: "denied", summary: "apply blocked by approver", entryId: "a1",
};
const ev2: AuditEvent = {
  time: "2026-06-01T12:05:00Z", sessionNamespace: "default", sessionName: "s2",
  agentClass: "chat-bot", kind: "tool_call", actor: "user:def", tool: "web_search",
  outcome: "allowed", summary: "search ran fine", entryId: "a2",
};

// stub returns both events for the query and a minimal facet payload (only
// kind+outcome) so facet values don't collide with table-cell text.
beforeEach(() => {
  vi.stubGlobal("fetch", vi.fn(async (url: RequestInfo | URL) => {
    const u = String(url);
    if (u.endsWith("/audit/query"))
      return new Response(JSON.stringify({ events: [ev1, ev2], hasMore: false, truncated: false }), { status: 200 });
    if (u.endsWith("/audit/facets"))
      return new Response(JSON.stringify({ counts: { kind: { approval: 1, tool_call: 1 }, outcome: { denied: 1, allowed: 1 } }, truncated: false }), { status: 200 });
    return new Response("{}", { status: 200 });
  }));
});

describe("AuditExplorer", () => {
  it("renders a color legend for each field (kind/outcome/agent/tool/actor)", async () => {
    render(<AuditExplorer apiBase="/admin/api" initialFilter={{}} />);
    const legend = await screen.findByRole("group", { name: "Color legends" });
    for (const label of ["Kind", "Outcome", "Agent", "Tool", "Actor"]) {
      expect(within(legend).getByText(label)).toBeTruthy();
    }
    // each colored value shows a chip in its legend (e.g. both agent classes).
    expect(within(legend).getByText("deploy-agent")).toBeTruthy();
    expect(within(legend).getByText("chat-bot")).toBeTruthy();
  });

  it("typed free-text filter narrows the loaded events", async () => {
    render(<AuditExplorer apiBase="/admin/api" initialFilter={{}} />);
    const table = await screen.findByRole("table");
    await waitFor(() => expect(within(table).getByText("web_search")).toBeTruthy());
    expect(within(table).getByText("terraform_apply")).toBeTruthy();

    const input = screen.getByPlaceholderText(/Filter loaded events/);
    fireEvent.change(input, { target: { value: "chat-bot" } });

    await waitFor(() => expect(within(table).queryByText("terraform_apply")).toBeNull());
    expect(within(table).getByText("web_search")).toBeTruthy();
    expect(screen.getByText(/1 of 2 shown/)).toBeTruthy();
  });

  it("pivots a filter on facet click", async () => {
    render(<AuditExplorer apiBase="/admin/api" initialFilter={{}} />);
    fireEvent.click(await screen.findByText(/denied \(1\)/));
    await waitFor(() => expect(screen.getByText(/outcome: denied/)).toBeTruthy(), { timeout: 2000 });
  });
});
