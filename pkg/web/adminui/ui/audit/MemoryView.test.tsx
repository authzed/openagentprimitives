import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import * as React from "react";
import { MemoryView } from "./MemoryView";
import type { MemoryData } from "../lib/api";

afterEach(cleanup);

const rollupData: MemoryData = {
  rollups: [
    { kind: "transcript", entries: 120, scopes: 8, lastWrite: "2026-06-10T12:00:00Z", appendOnly: true },
    { kind: "label", entries: 30, scopes: 4, lastWrite: "2026-06-10T11:00:00Z", appendOnly: false },
  ],
  truncated: false,
};

const searchData: MemoryData = {
  rollups: [],
  entries: [
    { kind: "transcript", scope: "default/s1", id: "transcript-7", created: "2026-06-10T12:00:00Z", score: 0.91 },
  ],
  truncated: false,
};

function stubFetch() {
  vi.stubGlobal("fetch", vi.fn(async (url: RequestInfo | URL) => {
    const body = String(url).includes("q=") ? searchData : rollupData;
    return new Response(JSON.stringify(body), { status: 200 });
  }));
}

describe("MemoryView", () => {
  it("renders per-kind rollups with an append-only badge", async () => {
    stubFetch();
    render(<MemoryView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText("transcript")).toBeTruthy());
    expect(screen.getByText("120")).toBeTruthy();
    expect(screen.getByText("append-only")).toBeTruthy();
    expect(screen.getByText("label")).toBeTruthy();
  });

  it("runs a search and renders ranked entries with scores", async () => {
    stubFetch();
    render(<MemoryView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText("transcript")).toBeTruthy());

    fireEvent.change(screen.getByLabelText("Search memory"), { target: { value: "deploy" } });
    fireEvent.submit(screen.getByLabelText("Search memory").closest("form")!);

    await waitFor(() => expect(screen.getByText("transcript-7")).toBeTruthy());
    expect(screen.getByText("default/s1")).toBeTruthy();
    expect(screen.getByText("0.91")).toBeTruthy();
  });

  it("shows an error state when the fetch fails", async () => {
    vi.stubGlobal("fetch", vi.fn(async () =>
      new Response(JSON.stringify({ error: "down" }), { status: 500 })));
    render(<MemoryView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText(/Could not load memory: down/)).toBeTruthy());
  });
});
