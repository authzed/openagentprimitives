import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import * as React from "react";
import { ResourceTable } from "./ResourceTable";
import type { ColumnSpec } from "./columns";
import type { ResourceRow } from "../lib/api";

const columns: ColumnSpec[] = [
  { header: "Kind", source: "badge", key: "kind" },
  { header: "Tools", source: "count", align: "right" },
];

const rows: ResourceRow[] = [
  {
    name: "alpha-server", namespace: "team-a", scope: "namespaced", status: "Valid",
    badges: [{ key: "kind", value: "mcpserver" }],
    counts: [{ label: "observedTools", value: 3 }],
    manageCmd: "kubectl -n team-a edit mcpserver alpha-server",
  },
  {
    name: "beta-box", namespace: "team-b", scope: "namespaced", status: "Failed",
    statusReason: "image pull error",
    badges: [{ key: "kind", value: "sidecartoolbox" }],
    counts: [{ label: "observedTools", value: 0 }],
    manageCmd: "kubectl -n team-b edit sidecartoolbox beta-box",
  },
];

afterEach(() => cleanup());

describe("ResourceTable", () => {
  it("renders a row per resource with its name, badge, and count cells", () => {
    render(<ResourceTable rows={rows} columns={columns} />);
    expect(screen.getByText("alpha-server")).toBeTruthy();
    expect(screen.getByText("beta-box")).toBeTruthy();
    expect(screen.getByText("mcpserver")).toBeTruthy();
    expect(screen.getByText("sidecartoolbox")).toBeTruthy();
    // a copy-to-clipboard button per row's manageCmd
    expect(screen.getAllByLabelText("Copy command")).toHaveLength(2);
  });

  it("narrows rows by the name filter box", () => {
    render(<ResourceTable rows={rows} columns={columns} />);
    fireEvent.change(screen.getByLabelText("Filter by name"), { target: { value: "alpha" } });
    expect(screen.getByText("alpha-server")).toBeTruthy();
    expect(screen.queryByText("beta-box")).toBeNull();
  });

  it("narrows rows by clicking a status filter chip", () => {
    render(<ResourceTable rows={rows} columns={columns} />);
    // Valid + Failed are distinct → status chips render; click Failed to narrow.
    fireEvent.click(screen.getByRole("button", { name: "Failed" }));
    expect(screen.getByText("beta-box")).toBeTruthy();
    expect(screen.queryByText("alpha-server")).toBeNull();
  });

  it("shows the empty state when there are no rows", () => {
    render(<ResourceTable rows={[]} columns={columns} emptyText="No tools configured yet." />);
    expect(screen.getByText("No tools configured yet.")).toBeTruthy();
  });

  it("links each row's Name to the entity detail page when entity is set", () => {
    render(<ResourceTable rows={rows} columns={columns} entity="tool" />);
    const link = screen.getByRole("link", { name: "alpha-server" });
    expect(link.getAttribute("href")).toBe("/admin/tool/team-a/alpha-server");
  });

  it("renders a plain (non-link) Name when entity is unset", () => {
    render(<ResourceTable rows={rows} columns={columns} />);
    expect(screen.queryByRole("link", { name: "alpha-server" })).toBeNull();
  });

  it("narrows rows by a badge facet filter", () => {
    render(<ResourceTable rows={rows} columns={columns} facet={{ key: "kind", label: "Kind" }} />);
    // Clicking the sidecartoolbox facet chip keeps only beta-box.
    fireEvent.click(screen.getByRole("button", { name: "sidecartoolbox" }));
    expect(screen.getByText("beta-box")).toBeTruthy();
    expect(screen.queryByText("alpha-server")).toBeNull();
  });
});
