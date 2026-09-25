import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import * as React from "react";
import { DirectoryView } from "./DirectoryView";

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

function stub(rows: unknown) {
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(rows), { status: 200 })));
}

const row = {
  name: "acme-github",
  namespace: "default",
  scope: "namespaced",
  status: "Ready",
  statusReason: "Synced",
  badges: [
    { key: "kind", value: "github" },
    { key: "credential", value: "forge-identity/forge-pat" },
  ],
  counts: [
    { label: "scopes", value: 3 },
    { label: "written", value: 40 },
    { label: "pruned", value: 2 },
    { label: "joinMisses", value: 7 },
  ],
  manageCmd: "oap directory configure",
};

describe("DirectoryView", () => {
  it("shows each sync's kind and its last pass's join misses", async () => {
    stub([row]);
    render(<DirectoryView apiBase="/admin/api" />);

    expect(await screen.findByText("acme-github")).toBeTruthy();
    expect(screen.getByText("github")).toBeTruthy();
    // The join-miss cell is the reason this panel exists: a growing value is how
    // a broken identity join announces itself.
    expect(screen.getByText("7")).toBeTruthy();
  });

  it("renders the empty state rather than a blank table", async () => {
    stub([]);
    render(<DirectoryView apiBase="/admin/api" />);

    expect(await screen.findByText(/No directory sources configured yet/i)).toBeTruthy();
  });
});
