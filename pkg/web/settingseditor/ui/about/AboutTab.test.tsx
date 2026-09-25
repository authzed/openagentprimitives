import "@testing-library/jest-dom/vitest";
import { render, screen, waitFor, cleanup, within } from "@testing-library/react";
import { describe, expect, it, vi, afterEach } from "vitest";
import { AboutTab } from "./AboutTab";
import type { InstallInfoResponse } from "../lib/api";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function stubFetch(body: unknown, status = 200) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } })),
  );
}

const fixture: InstallInfoResponse = {
  clusterDown: false,
  clusterKind: "desktop",
  components: [
    { name: "operator", image: "spicebox-operator:dev", ready: "1/1" },
    { name: "webd", image: "agentprimitives-webd:dev", ready: "1/1" },
  ],
  operatorEnv: [
    { name: "AP_CLUSTER_KIND", value: "desktop" },
    { name: "MEMORY_BACKEND", value: "sqlite" },
  ],
  node: { name: "desktop-node", kubeletVersion: "v1.29.4", ready: true },
  appVersion: "0.42.0",
};

describe("AboutTab", () => {
  it("renders every fixture section: cluster kind, components, operator env, node, app version", async () => {
    stubFetch(fixture);
    render(<AboutTab apiBase="/api" />);

    await waitFor(() => expect(screen.getByTestId("about-cluster-kind")).toBeInTheDocument());
    expect(within(screen.getByTestId("about-cluster-kind")).getByText("desktop")).toBeInTheDocument();

    const componentsTable = screen.getByTestId("about-components-table");
    expect(within(componentsTable).getByText("operator")).toBeInTheDocument();
    expect(within(componentsTable).getByText("spicebox-operator:dev")).toBeInTheDocument();
    expect(within(componentsTable).getByText("webd")).toBeInTheDocument();
    expect(within(componentsTable).getByText("agentprimitives-webd:dev")).toBeInTheDocument();
    expect(within(componentsTable).getAllByText("1/1")).toHaveLength(2);

    const nodeSection = screen.getByTestId("about-node");
    expect(within(nodeSection).getByText(/desktop-node/)).toBeInTheDocument();
    expect(within(nodeSection).getByText(/v1\.29\.4/)).toBeInTheDocument();
    expect(within(nodeSection).getByText(/Ready/)).toBeInTheDocument();

    expect(screen.getByTestId("about-app-version")).toHaveTextContent("0.42.0");
  });

  it("renders exactly the fixture's operator env rows — no name or value outside the fixture allowlist", async () => {
    stubFetch(fixture);
    render(<AboutTab apiBase="/api" />);
    await waitFor(() => expect(screen.getByTestId("about-env-table")).toBeInTheDocument());

    const envTable = screen.getByTestId("about-env-table");
    const rows = within(envTable).getAllByRole("row");
    // header row + one row per fixture entry, nothing synthesized.
    expect(rows).toHaveLength(1 + fixture.operatorEnv.length);
    for (const entry of fixture.operatorEnv) {
      const row = within(envTable).getByText(entry.name).closest("tr");
      expect(row).not.toBeNull();
      expect(within(row as HTMLElement).getByText(entry.value)).toBeInTheDocument();
    }
  });

  it("shows an Alert and no sections when the cluster is down", async () => {
    stubFetch({ clusterDown: true, clusterKind: "", components: [], operatorEnv: [], appVersion: "" });
    render(<AboutTab apiBase="/api" />);
    await waitFor(() => expect(screen.getByText(/desktop cluster isn.t running/i)).toBeInTheDocument());
    expect(screen.queryByTestId("about-components-table")).not.toBeInTheDocument();
  });

  it("renders a load error instead of crashing", async () => {
    stubFetch({ error: "settings: failed to load install info" }, 500);
    render(<AboutTab apiBase="/api" />);
    await waitFor(() => expect(screen.getByText(/failed to load install info/i)).toBeInTheDocument());
  });
});
