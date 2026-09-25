import { render, screen, fireEvent, waitFor, cleanup } from "@testing-library/react";
import { describe, expect, it, vi, afterEach } from "vitest";
import { BudgetView } from "./BudgetView";

const BUDGET = {
  byModel: [
    { key: "claude-opus-4-8", inputTokens: 1_000_000, outputTokens: 180_000, estimatedCostUSD: 30 },
    { key: "claude-sonnet-4-6", inputTokens: 400_000, outputTokens: 60_000, estimatedCostUSD: 12.18 },
  ],
  byAgentClass: [{ key: "support-bot", inputTokens: 800_000, outputTokens: 120_000, estimatedCostUSD: 24 }],
  bySession: [{ key: "default/s1", inputTokens: 500_000, outputTokens: 90_000, estimatedCostUSD: 18 }],
  byUser: [{ key: "user:alice", inputTokens: 900_000, outputTokens: 140_000, estimatedCostUSD: 28 }],
  estimated: true,
};

function stubFetch(status = 200, body: unknown = BUDGET) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith("/budget")) return new Response(JSON.stringify(body), { status });
      return new Response(JSON.stringify({ error: "not found" }), { status: 404 });
    }),
  );
}

describe("BudgetView", () => {
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    window.history.pushState({}, "", "/admin");
  });

  it("renders the By-model breakdown and the total estimated spend", async () => {
    stubFetch();
    render(<BudgetView apiBase="/admin/api" />);

    // By-model rows (default tab).
    await waitFor(() => expect(screen.getByText("claude-opus-4-8")).toBeTruthy());
    expect(screen.getByText("claude-sonnet-4-6")).toBeTruthy();
    // Total estimated spend = sum of byModel costs = $42.18.
    expect(screen.getByText("$42.18")).toBeTruthy();
    // A cost cell is labeled/estimated.
    expect(screen.getByText("$30.00")).toBeTruthy();
  });

  it("renders an unpriced (null) cost as NaN and poisons the total", async () => {
    stubFetch(200, {
      ...BUDGET,
      byModel: [
        { key: "claude-opus-4-8", inputTokens: 1_000_000, outputTokens: 180_000, estimatedCostUSD: 30 },
        { key: "custom-model", inputTokens: 400_000, outputTokens: 60_000, estimatedCostUSD: null },
      ],
    });
    render(<BudgetView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText("custom-model")).toBeTruthy());
    // The unpriced row's cell and the grand total both read "NaN" (one null
    // poisons the sum) — never a misleading $0.00.
    expect(screen.getAllByText("NaN").length).toBeGreaterThanOrEqual(2);
    expect(screen.queryByText("$0.00")).toBeNull();
  });

  it("switches to the By-user tab and shows its rows", async () => {
    stubFetch();
    render(<BudgetView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText("claude-opus-4-8")).toBeTruthy());
    fireEvent.click(screen.getByRole("tab", { name: /by user/i }));
    await waitFor(() => expect(screen.getByText("user:alice")).toBeTruthy());
  });

  it("color-codes each breakdown key with a stable swatch", async () => {
    stubFetch();
    const { container } = render(<BudgetView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText("claude-opus-4-8")).toBeTruthy());
    // One colored swatch per byModel row (2 in the fixture).
    const swatches = container.querySelectorAll('span[aria-hidden="true"][style*="background-color"]');
    expect(swatches.length).toBeGreaterThanOrEqual(2);
  });

  it("decodes the byUser key to a human email", async () => {
    // base64url("alice@example.com")
    stubFetch(200, {
      ...BUDGET,
      byUser: [{ key: "user:YWxpY2VAZXhhbXBsZS5jb20", inputTokens: 1, outputTokens: 1, estimatedCostUSD: 1 }],
    });
    render(<BudgetView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText("claude-opus-4-8")).toBeTruthy());
    fireEvent.click(screen.getByRole("tab", { name: /by user/i }));
    await waitFor(() => expect(screen.getByText("alice@example.com")).toBeTruthy());
  });

  it("roving-tabindex arrow keys move tab selection (Right → By agent, Home → By model)", async () => {
    stubFetch();
    render(<BudgetView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText("claude-opus-4-8")).toBeTruthy());

    const byModel = screen.getByRole("tab", { name: /by model/i });
    const byAgent = screen.getByRole("tab", { name: /by agent/i });
    // Roving tabindex: only the active tab is in the tab order.
    expect(byModel.getAttribute("tabindex")).toBe("0");
    expect(byAgent.getAttribute("tabindex")).toBe("-1");
    // The active panel is a labelled tabpanel controlled by its tab.
    const panel = screen.getByRole("tabpanel");
    expect(panel.getAttribute("aria-labelledby")).toBe("budget-tab-byModel");
    expect(byModel.getAttribute("aria-controls")).toBe("budget-panel-byModel");

    // ArrowRight advances selection to the By-agent tab and shows its rows.
    fireEvent.keyDown(byModel, { key: "ArrowRight" });
    await waitFor(() => expect(screen.getByText("support-bot")).toBeTruthy());
    expect(screen.getByRole("tab", { name: /by agent/i }).getAttribute("aria-selected")).toBe("true");
    expect(screen.getByRole("tab", { name: /by agent/i }).getAttribute("tabindex")).toBe("0");

    // Home jumps back to the first (By-model) tab.
    fireEvent.keyDown(screen.getByRole("tab", { name: /by agent/i }), { key: "Home" });
    await waitFor(() => expect(screen.getByText("claude-opus-4-8")).toBeTruthy());
    expect(screen.getByRole("tab", { name: /by model/i }).getAttribute("aria-selected")).toBe("true");
  });

  it("highlights the row named by a ?model= deep link", async () => {
    window.history.pushState({}, "", "/admin/budget?model=claude-sonnet-4-6");
    stubFetch();
    render(<BudgetView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText("from Overview")).toBeTruthy());
  });

  it("shows an error banner when the budget fetch fails", async () => {
    stubFetch(500, { error: "boom" });
    render(<BudgetView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText(/Could not load budget/)).toBeTruthy());
  });
});
