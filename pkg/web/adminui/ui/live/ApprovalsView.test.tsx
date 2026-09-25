import { render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import * as React from "react";
import { ApprovalsView } from "./ApprovalsView";

afterEach(() => vi.restoreAllMocks());

describe("ApprovalsView", () => {
  it("renders pending approvals of different kinds with tool + requester", async () => {
    vi.stubGlobal("fetch", vi.fn(async () =>
      new Response(JSON.stringify([
        { kind: "tool_call", tool: "code_gh", namespace: "default", name: "sess-a", agentClass: "deploy-agent", requester: "user:alice", requestedAt: "2026-06-30T12:00:00Z" },
        { kind: "leakage", namespace: "team", name: "sess-b", agentClass: "support-bot", requester: "user:bob", requestedAt: "2026-06-30T12:01:00Z" },
      ]), { status: 200 })));

    render(<ApprovalsView apiBase="/admin/api" />);

    await waitFor(() => expect(screen.getByText("code_gh")).toBeTruthy());
    expect(screen.getByText("Tool call")).toBeTruthy();
    // A leakage approval has no tool, so "Leakage" appears as both the card
    // title fallback and the kind badge — assert at least one.
    expect(screen.getAllByText("Leakage").length).toBeGreaterThan(0);
    expect(screen.getByText("user:alice")).toBeTruthy();
    expect(screen.getByText("user:bob")).toBeTruthy();
    expect(screen.getByText("deploy-agent / default/sess-a")).toBeTruthy();
    // The read-only note is always present once there are rows.
    expect(screen.getByText(/decisions happen in the originating channel thread/)).toBeTruthy();
  });

  it("shows an empty state when nothing is pending", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("[]", { status: 200 })));
    render(<ApprovalsView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText(/Nothing awaiting a decision/)).toBeTruthy());
  });

  it("surfaces a server error in a banner", async () => {
    vi.stubGlobal("fetch", vi.fn(async () =>
      new Response(JSON.stringify({ error: "boom" }), { status: 500 })));
    render(<ApprovalsView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText(/Could not load approvals: boom/)).toBeTruthy());
  });
});
