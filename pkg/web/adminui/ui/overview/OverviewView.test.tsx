import { render, screen, waitFor, cleanup } from "@testing-library/react";
import { describe, expect, it, vi, afterEach } from "vitest";
import * as React from "react";
import { OverviewView } from "./OverviewView";

const OVERVIEW = {
  byModel: [
    { model: "claude-opus-4-8", inputTokens: 1_000_000, outputTokens: 180_000, sessions: 2 },
    { model: "claude-sonnet-4-6", inputTokens: 400_000, outputTokens: 60_000, sessions: 1 },
  ],
  byAgentClass: [{ class: "support-bot", inputTokens: 800_000, outputTokens: 120_000, sessions: 2 }],
  series24h: Array.from({ length: 24 }, (_, i) => ({
    hourStartUnix: 1_700_000_000 + i * 3600,
    inputTokens: i * 1000,
    outputTokens: i * 500,
  })),
  kpis: { activeSessions: 3, tokensToday: 1_840_000, toolCalls24h: 312, denials24h: 4, approvalsPending: 2 },
  computedAt: "2026-06-30T12:00:00Z",
  truncated: false,
  budget: { tokensSpent: 1_640_000, estimatedCostUSD: 42.18, estimated: true },
};

const HEALTH = {
  components: [
    { name: "operator", status: "Healthy", detail: "1/1 ready" },
    { name: "graphiti", status: "Degraded", detail: "unreachable" },
  ],
  rollup: { pods: 24, ready: 23, cpu: "n/a", memory: "n/a" },
};

function stubFetch() {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith("/overview")) return new Response(JSON.stringify(OVERVIEW), { status: 200 });
      if (url.endsWith("/health")) return new Response(JSON.stringify(HEALTH), { status: 200 });
      return new Response(JSON.stringify({ error: "not found" }), { status: 404 });
    }),
  );
}

describe("OverviewView", () => {
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("shows 'est. NaN' when the budget cost is unpriced (null)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        if (url.endsWith("/overview"))
          return new Response(
            JSON.stringify({ ...OVERVIEW, budget: { ...OVERVIEW.budget, estimatedCostUSD: null } }),
            { status: 200 },
          );
        if (url.endsWith("/health")) return new Response(JSON.stringify(HEALTH), { status: 200 });
        return new Response(JSON.stringify({ error: "not found" }), { status: 404 });
      }),
    );
    render(<OverviewView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText("est. NaN")).toBeTruthy());
  });

  it("renders KPIs, the token chart, a model bar, the est-cost label, and a health pill", async () => {
    stubFetch();
    const { container } = render(<OverviewView apiBase="/admin/api" />);

    // KPI scalars from the wire shape.
    await waitFor(() => expect(screen.getByText("1.84M")).toBeTruthy()); // tokens today
    expect(screen.getByText("312")).toBeTruthy(); // tool calls 24h

    // The 24h token area chart is an inline SVG.
    expect(container.querySelector("svg")).toBeTruthy();

    // A per-model bar/legend row renders the model id.
    expect(screen.getByText("claude-opus-4-8")).toBeTruthy();

    // The decorative proportion bar is hidden from assistive tech — the legend
    // ViewLinks below carry the accessible (keyboard + AT) path to the budget.
    expect(container.querySelector('div[aria-hidden="true"].overflow-hidden')).toBeTruthy();

    // The budget panel's estimated-cost label.
    expect(screen.getByText("est. $42.18")).toBeTruthy();

    // Cluster-health component pills.
    await waitFor(() => expect(screen.getByText("operator")).toBeTruthy());
    expect(screen.getByText("graphiti")).toBeTruthy();
    expect(screen.getByText("Degraded")).toBeTruthy();
  });

  it("links each agent-class row to the agent detail page (bare-name id)", async () => {
    stubFetch();
    render(<OverviewView apiBase="/admin/api" />);
    // The class key is a bare name (no namespace); the link resolves via the
    // useResourceRow bare-name match.
    const link = (await screen.findByText("support-bot")).closest("a") as HTMLAnchorElement;
    expect(link.getAttribute("href")).toBe("/admin/agent/support-bot");
  });

  it("shows an error banner when the overview fetch fails", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({ error: "boom" }), { status: 500 })));
    render(<OverviewView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText(/Could not load overview/)).toBeTruthy());
  });
});
