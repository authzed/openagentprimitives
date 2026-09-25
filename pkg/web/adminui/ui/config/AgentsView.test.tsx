import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import * as React from "react";
import { AgentsView } from "./AgentsView";
import { removeCookie } from "../lib/cookies";

const agents = [
  {
    name: "support-bot", namespace: "default", scope: "namespaced", status: "Valid",
    badges: [{ key: "identityMode", value: "per_session" }],
    counts: [{ label: "tools", value: 4 }, { label: "skills", value: 2 }],
    manageCmd: "kubectl -n default edit agentclass support-bot",
  },
  {
    name: "broken-bot", namespace: "default", scope: "namespaced", status: "Invalid",
    statusReason: "skill not resolved",
    counts: [{ label: "tools", value: 0 }, { label: "skills", value: 1 }],
    manageCmd: "kubectl -n default edit agentclass broken-bot",
  },
];

// The grid/table preference persists in a cookie — clear it between tests so
// each starts from the default grid view.
afterEach(() => { cleanup(); vi.restoreAllMocks(); removeCookie("agents_view"); });

function stubAgents() {
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(agents), { status: 200 })));
}

describe("AgentsView", () => {
  it("renders a card per AgentClass with status, tool/skill rollup, and identity mode", async () => {
    stubAgents();
    render(<AgentsView apiBase="/admin/api" />);

    await waitFor(() => expect(screen.getByText("support-bot")).toBeTruthy());
    expect(screen.getByText("broken-bot")).toBeTruthy();
    expect(screen.getByText("per_session")).toBeTruthy();
    expect(screen.getByText("4 tools · 2 skills")).toBeTruthy();
    // not-healthy card surfaces its reason
    expect(screen.getByText("skill not resolved")).toBeTruthy();
    // cards link to the Agent detail page
    expect(screen.getByRole("link", { name: /support-bot/ }).getAttribute("href")).toBe(
      "/admin/agent/default/support-bot",
    );
  });

  it("shows the Install agent entry point even before the list loads, and a completed install refetches the list", async () => {
    const fetchMock = vi
      .fn()
      // Initial /config/agents load.
      .mockResolvedValueOnce(new Response(JSON.stringify(agents), { status: 200 }))
      // POST /agents/oap-install.
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({ name: "new-bot", appliedKinds: ["AgentClass"], secretsCreated: 0 }),
          { status: 200 },
        ),
      )
      // Refetch of /config/agents after the install succeeds.
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify([
            ...agents,
            { name: "new-bot", namespace: "default", scope: "namespaced", status: "Valid" },
          ]),
          { status: 200 },
        ),
      );
    vi.stubGlobal("fetch", fetchMock);

    render(<AgentsView apiBase="/admin/api" />);
    // The install entry point is present immediately, before the list resolves.
    expect(screen.getByRole("button", { name: "Install agent" })).toBeTruthy();
    await waitFor(() => expect(screen.getByText("support-bot")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Install agent" }));
    fireEvent.change(await screen.findByLabelText("Bundle file"), {
      target: { files: [new File(["bytes"], "agent.oap")] },
    });
    fireEvent.click(screen.getByRole("button", { name: "Install" }));

    await waitFor(() => expect(screen.getByText(/Installed/)).toBeTruthy());
    expect(fetchMock).toHaveBeenCalledTimes(3); // initial list + install + refetch
  });

  it("toggles from the card grid to the table view (cookie-persisted)", async () => {
    stubAgents();
    render(<AgentsView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText("4 tools · 2 skills")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Table view" }));

    // Grid rollup text is gone; the table's column header appears instead.
    expect(screen.queryByText("4 tools · 2 skills")).toBeNull();
    expect(screen.getByText("Identity")).toBeTruthy();
    // The preference was written to the cookie.
    expect(document.cookie).toContain("ap_admin_agents_view=table");
  });

  it("surfaces a load error in a banner", async () => {
    vi.stubGlobal("fetch", vi.fn(async () =>
      new Response(JSON.stringify({ error: "boom" }), { status: 500 })));
    render(<AgentsView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText(/Could not load agents: boom/)).toBeTruthy());
  });

  it("table: identity links to the AgentIdentity, with a mode-colored dot per type", async () => {
    const identityAgents = [
      {
        name: "svc-bot", namespace: "default", scope: "namespaced", status: "Valid",
        badges: [{ key: "identityMode", value: "agent" }, { key: "identityRef", value: "gh-bot" }],
        counts: [{ label: "tools", value: 1 }, { label: "skills", value: 0 }],
        manageCmd: "kubectl -n default edit agentclass svc-bot",
      },
      {
        name: "pass-bot", namespace: "default", scope: "namespaced", status: "Valid",
        badges: [{ key: "identityMode", value: "userPassthrough" }],
        counts: [{ label: "tools", value: 2 }, { label: "skills", value: 1 }],
        manageCmd: "kubectl -n default edit agentclass pass-bot",
      },
    ];
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(identityAgents), { status: 200 })));
    render(<AgentsView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText("svc-bot")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Table view" }));
    await waitFor(() => expect(screen.getByText("Identity")).toBeTruthy());

    // The bound AgentIdentity links to its detail page.
    expect(screen.getByRole("link", { name: "gh-bot" }).getAttribute("href")).toBe(
      "/admin/identity/default/gh-bot",
    );
    // The two modes render distinctly-colored dots (agent vs userPassthrough).
    const agentDot = screen.getByTitle("agent");
    const passDot = screen.getByTitle("userPassthrough");
    expect(agentDot.className).toContain("bg-state");
    expect(passDot.className).toContain("bg-success");
    expect(agentDot.className).not.toBe(passDot.className);
  });
});
