import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { KnowledgeView } from "./KnowledgeView";
import type { KGCommunity, KGFact } from "../lib/api";

afterEach(cleanup);

const communities: KGCommunity[] = [
  { uuid: "c1", name: "infra-team", summary: "infra cluster", members: ["e1", "e2"] },
];

const facts: KGFact[] = [
  { uuid: "f1", name: "deployed", fact: "alice deployed infra", fromEntity: "e1", toEntity: "e2" },
];

// stubKG dispatches by URL: /kg/communities is the availability probe, /kg/search
// returns scripted facts. available drives the degraded panel.
function stubKG(opts: { available: boolean; note?: string }) {
  vi.stubGlobal("fetch", vi.fn(async (url: RequestInfo | URL) => {
    const u = String(url);
    if (u.includes("/kg/communities")) {
      return new Response(
        JSON.stringify({ available: opts.available, note: opts.note, communities: opts.available ? communities : [] }),
        { status: 200 },
      );
    }
    if (u.includes("/kg/search")) {
      return new Response(JSON.stringify({ available: true, facts }), { status: 200 });
    }
    return new Response(JSON.stringify({ available: opts.available }), { status: 200 });
  }));
}

describe("KnowledgeView", () => {
  it("available: searches facts and renders the results", async () => {
    stubKG({ available: true });
    render(<KnowledgeView apiBase="/admin/api" />);

    // The communities probe resolves first; the section renders.
    await waitFor(() => expect(screen.getByText("infra-team")).toBeTruthy());

    fireEvent.change(screen.getByLabelText("Search facts"), { target: { value: "deploy" } });
    fireEvent.submit(screen.getByLabelText("Search facts").closest("form")!);

    await waitFor(() => expect(screen.getByText("alice deployed infra")).toBeTruthy());
  });

  it("not configured: renders the degraded panel, not an error", async () => {
    stubKG({ available: false, note: "knowledge graph not configured (set --graphiti-endpoint)" });
    render(<KnowledgeView apiBase="/admin/api" />);

    await waitFor(() => expect(screen.getByText("Knowledge graph not configured")).toBeTruthy());
    expect(screen.getAllByText(/--graphiti-endpoint/).length).toBeGreaterThan(0);
    // No fact-search box in the degraded state.
    expect(screen.queryByLabelText("Search facts")).toBeNull();
  });
});
