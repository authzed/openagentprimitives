import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { AgentPage } from "./AgentPage";
import type { ResourceDetail, ResourceRow } from "../../lib/api";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});
beforeEach(() => window.history.pushState({}, "", "/admin/agent/default/support-bot"));

// routeFetch dispatches a stubbed response per URL suffix so a page that fetches
// several endpoints (the config detail + /sessions) gets the right body for each.
function routeFetch(routes: Record<string, unknown>) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) => {
      for (const [suffix, body] of Object.entries(routes)) {
        if (url.endsWith(suffix)) return new Response(JSON.stringify(body), { status: 200 });
      }
      return new Response(JSON.stringify({ error: `no stub for ${url}` }), { status: 404 });
    }),
  );
}

const AGENT_DETAIL: ResourceDetail = {
  name: "support-bot",
  namespace: "default",
  scope: "namespaced",
  status: "Valid",
  description: "Answers customer questions.",
  manageCmd: "kubectl edit agentclass support-bot -n default",
  sections: [
    { id: "prompt", label: "Prompt", kind: "text", text: "You are helpful." },
    { id: "tools", label: "Tools", kind: "list", items: [{ title: "github" }] },
    {
      id: "identity",
      label: "Identity",
      kind: "fields",
      fields: [{ label: "Identity mode", value: "per_session" }],
    },
  ],
};

// AGENTS_LIST is the /config/agents projector table — what a bare-name link
// resolves through to recover the namespace of a namespaced AgentClass.
const AGENTS_LIST: ResourceRow[] = [
  { name: "support-bot", namespace: "default", scope: "namespaced", status: "Valid" },
];

describe("AgentPage", () => {
  it("Overview: shows the description, status, manage command, and the Prompt + Sessions tabs", async () => {
    routeFetch({ "/config/agents/default/support-bot": AGENT_DETAIL, "/sessions": [] });

    render(<AgentPage apiBase="/admin/api" id="default/support-bot" />);

    await waitFor(() => expect(screen.getByText("support-bot")).toBeTruthy());
    // Overview body: prominent description, status pill, manage command
    expect(screen.getByText("Answers customer questions.")).toBeTruthy();
    // status word appears in the header pill AND the Overview status summary
    expect(screen.getAllByText("Valid").length).toBeGreaterThan(0);
    expect(screen.getByText("kubectl edit agentclass support-bot -n default")).toBeTruthy();
    // The synthetic Sessions tab + a backend Prompt section tab are both present.
    expect(screen.getByRole("tab", { name: "Sessions" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "Prompt" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "Overview" })).toBeTruthy();
  });

  it("Sessions tab: renders a TABLE of only this class's live sessions, each linked to its detail", async () => {
    routeFetch({
      "/config/agents/default/support-bot": AGENT_DETAIL,
      "/sessions": [
        { namespace: "default", name: "sess-1", class: "support-bot", turnCount: 0, phase: "Running" },
        { namespace: "default", name: "other", class: "billing-bot", turnCount: 0 },
      ],
    });

    render(<AgentPage apiBase="/admin/api" id="default/support-bot" tab="sessions" />);

    const link = (await screen.findByText("sess-1")).closest("a") as HTMLAnchorElement;
    expect(link.getAttribute("href")).toBe("/admin/session/default/sess-1");
    expect(screen.queryByText("other")).toBeNull();
    // Rendered as a table (columns), not a bare list.
    expect(screen.getByRole("columnheader", { name: "Session" })).toBeTruthy();
    expect(screen.getByRole("columnheader", { name: "Phase" })).toBeTruthy();
    expect(screen.getByRole("columnheader", { name: "Started" })).toBeTruthy();
    // Phase renders as a color-coded pill.
    const phase = screen.getByText("Running");
    expect(phase.className).toContain("rounded-full");
  });

  it("degraded: surfaces a warning box carrying the full statusReason on the Overview", async () => {
    routeFetch({
      "/config/agents/default/support-bot": {
        ...AGENT_DETAIL,
        status: "Invalid",
        statusReason: "skill acme//x@main not resolved",
      },
      "/sessions": [],
    });

    render(<AgentPage apiBase="/admin/api" id="default/support-bot" />);

    const box = await screen.findByRole("alert");
    expect(box.textContent).toContain("skill acme//x@main not resolved");
  });

  // The Overview "Tokens by agent class" bars and the Budget "By agent class"
  // keys deep-link with a BARE class name (no namespace). ConfigDetail must list
  // agents, match the row, recover its namespace, and fetch the ns/name detail —
  // otherwise the two most prominent screens produce dead "Not found" links.
  it("bare-name link: lists agents, matches the row, and resolves to the ns/name detail page", async () => {
    routeFetch({
      "/config/agents/default/support-bot": AGENT_DETAIL,
      "/config/agents": AGENTS_LIST,
      "/sessions": [],
    });

    render(<AgentPage apiBase="/admin/api" id="support-bot" />);

    // Resolved page renders (the AgentClass name + its description), NOT "Not found".
    await waitFor(() => expect(screen.getByText("support-bot")).toBeTruthy());
    expect(screen.getByText("Answers customer questions.")).toBeTruthy();
    expect(screen.queryByText(/Not found/)).toBeNull();
  });

  it("bare name with no matching row: clean not-found (no crash)", async () => {
    // Only the list is stubbed; the fall-through direct detail fetch 404s.
    routeFetch({ "/config/agents": AGENTS_LIST });

    render(<AgentPage apiBase="/admin/api" id="ghost-bot" />);

    await waitFor(() => expect(screen.getByText(/Not found: ghost-bot/)).toBeTruthy());
  });
});
