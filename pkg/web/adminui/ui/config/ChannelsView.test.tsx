import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import * as React from "react";
import { ChannelsView } from "./ChannelsView";

const rows = [
  {
    name: "slack-a", namespace: "ns1", scope: "namespaced", status: "Connected",
    badges: [
      { key: "kind", value: "slack" },
      { key: "role", value: "both" },
      { key: "agentClass", value: "alpha" },
    ],
    manageCmd: "kubectl edit channel slack-a -n ns1",
  },
  {
    name: "events-sink", namespace: "ns1", scope: "namespaced", status: "Unknown",
    badges: [
      { key: "kind", value: "slack" },
      { key: "role", value: "monitoring" },
    ],
    manageCmd: "kubectl edit channel events-sink -n ns1",
  },
];

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

function stub() {
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(rows), { status: 200 })));
}

describe("ChannelsView", () => {
  it("shows a monitoring channel as 'monitoring', never 'Unknown'", async () => {
    stub();
    render(<ChannelsView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText("events-sink")).toBeTruthy());
    // Both the status pill and the role chip read "monitoring"; the raw
    // projector "Unknown" is gone.
    expect(screen.getAllByText("monitoring").length).toBeGreaterThan(0);
    expect(screen.queryByText("Unknown")).toBeNull();
  });

  it("colors the monitoring role muted and a live role with the accent", async () => {
    stub();
    render(<ChannelsView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText("both")).toBeTruthy());
    // A live input/output role is accented (distinct from monitoring).
    expect(screen.getByText("both").className).toContain("text-primary");
    // Every "monitoring" label (role chip + status pill) is muted.
    for (const el of screen.getAllByText("monitoring")) {
      expect(el.className).toContain("text-muted-foreground");
    }
  });

  it("links the agentClass to its Agent detail page", async () => {
    stub();
    render(<ChannelsView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText("slack-a")).toBeTruthy());
    const link = screen.getByRole("link", { name: "alpha" });
    expect(link.getAttribute("href")).toBe("/admin/agent/ns1/alpha");
  });
});
