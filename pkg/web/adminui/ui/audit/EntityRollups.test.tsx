import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import * as React from "react";
import { EntityRollups } from "./EntityRollups";
import type { EntityRow } from "../lib/api";

afterEach(cleanup);

function stub(rows: EntityRow[]) {
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(rows), { status: 200 })));
}

describe("EntityRollups", () => {
  it("agents axis: key links to the agent detail; 'filter logs' drills to the audit tab", async () => {
    stub([{ key: "deploy-agent", events: 88, denied: 3 }]);
    const onDrill = vi.fn();
    render(<EntityRollups apiBase="/admin/api" axis="agents" onDrill={onDrill} />);

    const link = (await screen.findByText("deploy-agent")) as HTMLAnchorElement;
    expect(link.getAttribute("href")).toBe("/admin/agent/deploy-agent");
    expect(screen.getByText("88")).toBeTruthy();

    fireEvent.click(screen.getByText("filter logs"));
    expect(onDrill).toHaveBeenCalledWith({ agentClass: "deploy-agent" });
  });

  it("sessions axis: shows status/est-tokens/started time + a linked, colored started-by, and links to the session page", async () => {
    const fiveMinAgo = new Date(Date.now() - 5 * 60 * 1000).toISOString();
    stub([{
      key: "default/sess-7f2a", events: 42, denied: 1,
      status: "Running", inputTokens: 1200, outputTokens: 500, estimatedCostUSD: 0.03,
      startedBy: "user:YWxpY2VAZXhhbXBsZS5jb20", startedAt: fiveMinAgo,
    }]);
    const onDrill = vi.fn();
    render(<EntityRollups apiBase="/admin/api" axis="sessions" onDrill={onDrill} />);

    // Default namespace hidden in the label; the route id keeps the full ns/name.
    const link = (await screen.findByText("sess-7f2a")).closest("a") as HTMLAnchorElement;
    expect(link.getAttribute("href")).toBe("/admin/session/default/sess-7f2a");
    // enriched columns
    expect(screen.getByText("Running")).toBeTruthy();
    expect(screen.getByText(/1\.2K in · 500 out/)).toBeTruthy();
    // Started column: humanized relative time.
    expect(screen.getByText(/ago|just now/)).toBeTruthy();
    // Started-by is decoded, linked to the user detail, and carries a stable color dot.
    const startedBy = screen.getByText("alice@example.com").closest("a") as HTMLAnchorElement;
    expect(startedBy.getAttribute("href")).toBe("/admin/user/user:YWxpY2VAZXhhbXBsZS5jb20");
    const cell = startedBy.closest("td") as HTMLElement;
    expect(cell.querySelector('span[aria-hidden="true"][style*="background-color"]')).toBeTruthy();

    fireEvent.click(screen.getByText("filter logs"));
    expect(onDrill).toHaveBeenCalledWith({ sessionNamespace: "default", sessionName: "sess-7f2a" });
  });

  it("tools axis: key links to the tool detail", async () => {
    stub([{ key: "terraform_apply", events: 12, denied: 0 }]);
    render(<EntityRollups apiBase="/admin/api" axis="tools" onDrill={vi.fn()} />);
    const link = (await screen.findByText("terraform_apply")) as HTMLAnchorElement;
    expect(link.getAttribute("href")).toBe("/admin/tool/terraform_apply");
  });

  it("users axis: decodes the subject for display but links by the raw key", async () => {
    stub([{ key: "user:YWxpY2VAZXhhbXBsZS5jb20", events: 5, denied: 0 }]);
    render(<EntityRollups apiBase="/admin/api" axis="users" onDrill={vi.fn()} />);
    const link = (await screen.findByText("alice@example.com")) as HTMLAnchorElement;
    expect(link.getAttribute("href")).toBe("/admin/user/user:YWxpY2VAZXhhbXBsZS5jb20");
  });
});
