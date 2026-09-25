import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ToolsView } from "./ToolsView";

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

const tools = [
  {
    name: "gh", namespace: "default", scope: "namespaced", status: "Degraded",
    statusReason: "unreachable: dial tcp timeout",
    badges: [{ key: "kind", value: "mcpserver" }],
    counts: [{ label: "tools", value: 3 }],
    manageCmd: "kubectl -n default edit mcpserver gh",
  },
  {
    name: "reader", namespace: "default", scope: "namespaced", status: "Valid",
    badges: [{ key: "kind", value: "toolspec" }],
    counts: [{ label: "tools", value: 1 }],
    manageCmd: "kubectl -n default edit spiceboxtoolspec reader",
  },
];

describe("ToolsView", () => {
  it("degraded status pill reveals its statusReason in a tooltip on hover", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(tools), { status: 200 })));
    render(<ToolsView apiBase="/admin/api" />);
    // The status pill lives in a table cell (the same word also appears as a
    // StatusFilter chip button, so scope to the <td>).
    const pillOf = () => screen.getAllByText("Degraded").find((el) => el.closest("td"));
    await waitFor(() => expect(pillOf()).toBeTruthy());

    // The reason is not visible until the pill is hovered/focused.
    expect(screen.queryByText(/dial tcp timeout/)).toBeNull();

    const pill = pillOf() as HTMLElement;
    fireEvent.focus(pill);
    fireEvent.pointerEnter(pill);

    await waitFor(() =>
      expect(screen.getAllByText(/unreachable: dial tcp timeout/).length).toBeGreaterThan(0),
    );
  });
});
