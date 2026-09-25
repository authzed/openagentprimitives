import { render, screen, waitFor, cleanup } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ClusterBadge } from "./ClusterBadge";
import type { ClusterInfo } from "../lib/api";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

// stubCluster wires the global fetch to resolve /cluster with the given
// (partial) ClusterInfo, defaulting the required fields.
function stubCluster(info: Partial<ClusterInfo>) {
  const full: ClusterInfo = { name: "", type: "unknown", consoleURL: "", ...info };
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(full), { status: 200 })));
}

describe("ClusterBadge", () => {
  it("renders the cluster name and the per-type label + icon", async () => {
    stubCluster({ name: "ap-prod", type: "gke", consoleURL: "" });
    const { container } = render(<ClusterBadge apiBase="/admin/api" />);

    expect(await screen.findByText("ap-prod")).toBeTruthy();
    // The short type label surfaces the provider (assert on the label text).
    expect(screen.getByText("GKE")).toBeTruthy();
    // A per-type icon (inline SVG or lucide glyph) renders alongside it.
    expect(container.querySelector("svg")).toBeTruthy();
  });

  it("falls back to the type label when the name is empty", async () => {
    stubCluster({ name: "", type: "local", consoleURL: "" });
    render(<ClusterBadge apiBase="/admin/api" />);
    expect(await screen.findByText("Local")).toBeTruthy();
  });

  it("with a consoleURL, renders a scheme-guarded external link", async () => {
    const url = "https://console.cloud.google.com/kubernetes/clusters/details/us-central1-a/ap-prod?project=demo";
    stubCluster({ name: "ap-prod", type: "gke", consoleURL: url });
    render(<ClusterBadge apiBase="/admin/api" />);

    const link = (await screen.findByRole("link")) as HTMLAnchorElement;
    expect(link.getAttribute("href")).toBe(url);
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toBe("noopener noreferrer");
  });

  it("without a consoleURL, renders static text (no anchor)", async () => {
    stubCluster({ name: "ap-prod", type: "gke", consoleURL: "" });
    render(<ClusterBadge apiBase="/admin/api" />);
    await screen.findByText("ap-prod");
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("rejects a non-http console URL scheme — renders static text, not an anchor", async () => {
    // httpUrl must veto a javascript: href (XSS guard) → the badge stays text.
    stubCluster({ name: "ap-prod", type: "gke", consoleURL: "javascript:alert(1)" });
    render(<ClusterBadge apiBase="/admin/api" />);
    await screen.findByText("ap-prod");
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("degrades quietly on a fetch error — the header still renders", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => { throw new Error("network down"); }));
    render(
      <header>
        <span data-testid="brand">AP Admin Dashboard</span>
        <ClusterBadge apiBase="/admin/api" />
      </header>,
    );
    // The sibling header content is unaffected and no badge/link is rendered.
    expect(screen.getByTestId("brand")).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole("link")).toBeNull());
    expect(screen.queryByText("GKE")).toBeNull();
  });
});
