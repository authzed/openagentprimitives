import { render, screen, waitFor, cleanup } from "@testing-library/react";
import { describe, expect, it, vi, afterEach } from "vitest";
import { SettingsView } from "./SettingsView";

const SETTINGS = {
  rows: [
    { group: "Defaults", label: "Model", value: "claude-opus-4-8" },
    { group: "Limits", label: "Token ceiling", value: "2,000,000", note: "per session" },
    { group: "Limits", label: "Max tool calls", value: "50" },
  ],
  manageCmd: "oap settings show",
};

function stubFetch(body: unknown, status = 200) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith("/config/settings")) return new Response(JSON.stringify(body), { status });
      return new Response(JSON.stringify({ error: "not found" }), { status: 404 });
    }),
  );
}

describe("SettingsView", () => {
  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  it("renders settings as a table with Group/Setting/Value/Note columns, Limits first", async () => {
    stubFetch(SETTINGS);
    render(<SettingsView apiBase="/admin/api" />);

    await waitFor(() => expect(screen.getByRole("columnheader", { name: "Group" })).toBeTruthy());
    expect(screen.getByRole("columnheader", { name: "Setting" })).toBeTruthy();
    expect(screen.getByRole("columnheader", { name: "Value" })).toBeTruthy();
    expect(screen.getByRole("columnheader", { name: "Note" })).toBeTruthy();

    // Values + a note from the wire shape.
    expect(screen.getByText("claude-opus-4-8")).toBeTruthy();
    expect(screen.getByText("per session")).toBeTruthy();

    // Limits sorts before Defaults: the first group label cell reads "Limits".
    const groupCells = screen.getAllByRole("cell").filter((c) => /^(Limits|Defaults)$/.test(c.textContent ?? ""));
    expect(groupCells[0].textContent).toBe("Limits");

    // The manage command affordance is present.
    expect(screen.getByText("oap settings show")).toBeTruthy();
  });

  it("shows the not-found note when there is no ClusterAgentSettings", async () => {
    stubFetch({ rows: [], manageCmd: "oap settings show", note: "No ClusterAgentSettings configured." });
    render(<SettingsView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText("No ClusterAgentSettings configured.")).toBeTruthy());
  });

  it("shows an error banner when the settings fetch fails", async () => {
    stubFetch({ error: "boom" }, 500);
    render(<SettingsView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText(/Could not load settings/)).toBeTruthy());
  });
});
