import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { ToolPage } from "./ToolPage";
import type { ResourceDetail } from "../../lib/api";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});
beforeEach(() => window.history.pushState({}, "", "/admin/tool/default/github"));

function stubFetch(body: unknown, status = 200) {
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(body), { status })));
}

const BASE: ResourceDetail = {
  name: "github",
  namespace: "default",
  scope: "namespaced",
  status: "Reachable",
  manageCmd: "kubectl edit mcpserver github -n default",
  sections: [
    {
      id: "connection",
      label: "Connection",
      kind: "fields",
      fields: [{ label: "Endpoint", value: "https://mcp.example.com" }],
    },
  ],
};

describe("ToolPage", () => {
  it("surfaces a prominent degraded warning box carrying the full statusReason", async () => {
    stubFetch({
      ...BASE,
      status: "Degraded",
      statusReason: "MCP endpoint https://mcp.example.com unreachable: dial tcp i/o timeout",
    });

    render(<ToolPage apiBase="/admin/api" id="default/github" />);

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("Degraded");
    expect(alert.textContent).toContain("MCP endpoint https://mcp.example.com unreachable");
    // manage command still rendered on the Overview below the warning
    await waitFor(() => expect(screen.getByText("kubectl edit mcpserver github -n default")).toBeTruthy());
  });

  it("renders no warning box for a healthy tool, and exposes the Connection tab", async () => {
    stubFetch(BASE);

    render(<ToolPage apiBase="/admin/api" id="default/github" />);
    // status word appears in the header pill AND the Overview status summary
    await waitFor(() => expect(screen.getAllByText("Reachable").length).toBeGreaterThan(0));
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByRole("tab", { name: "Connection" })).toBeTruthy();
  });
});
