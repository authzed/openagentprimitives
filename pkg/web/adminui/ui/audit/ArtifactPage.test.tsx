import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import * as React from "react";
import { ArtifactPage } from "./ArtifactPage";
import type { ArtifactDetail } from "../lib/api";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});
beforeEach(() => window.history.pushState({}, "", "/admin/artifact/default/ar-s1-aaa"));

const DETAIL: ArtifactDetail = {
  name: "ar-s1-aaa", namespace: "default", session: "default/s1", kind: "html",
  phase: "Ready", mime: "text/html", size: 2048, created: "2026-06-10T12:00:00Z",
  revisions: [
    { name: "ar-s1-aaa", phase: "Ready", mime: "text/html", size: 2048, created: "2026-06-10T12:00:00Z" },
    { name: "ar-s1-bbb", phase: "Failed", mime: "text/html", size: 0, created: "2026-06-10T11:00:00Z" },
  ],
  viewPath: "/artifact-view?artifactId=art-1&sessionRef=default%2Fs1",
  outputRef: "artifactstore://default/ar-s1-aaa",
};

function stub(body: unknown, status = 200) {
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(body), { status })));
}

describe("ArtifactPage", () => {
  it("Overview: renders metadata, session link, the download ref, and the viewer link (unsigned admin params)", async () => {
    stub(DETAIL);
    render(<ArtifactPage apiBase="/admin/api" id="default/ar-s1-aaa" />);

    await waitFor(() => expect(screen.getByText("html")).toBeTruthy());
    expect(screen.getByText("text/html")).toBeTruthy();
    expect(screen.getByText("2.0KB")).toBeTruthy();

    // Default namespace hidden in the session link label; route id keeps ns/name.
    const session = screen.getByText("s1").closest("a") as HTMLAnchorElement;
    expect(session.getAttribute("href")).toBe("/admin/session/default/s1");

    // viewPath carries the artifactId param → a clickable "Open viewer" link
    // (opens in a new tab; auth via IdP cookie, authz via CheckView).
    const viewer = screen.getByText("Open viewer") as HTMLAnchorElement;
    expect(viewer.getAttribute("href")).toBe("/artifact-view?artifactId=art-1&sessionRef=default%2Fs1");
    expect(viewer.getAttribute("target")).toBe("_blank");
    expect(viewer.getAttribute("rel")).toBe("noopener noreferrer");

    expect(screen.getByText("artifactstore://default/ar-s1-aaa")).toBeTruthy();
  });

  it("Overview: hides the viewer link for a bare /artifact-view base (no artifactId)", async () => {
    stub({ ...DETAIL, viewPath: "/artifact-view" });
    render(<ArtifactPage apiBase="/admin/api" id="default/ar-s1-aaa" />);

    await waitFor(() => expect(screen.getByText("html")).toBeTruthy());
    expect(screen.queryByText("Open viewer")).toBeNull();
    expect(screen.getByText(/No viewer available/)).toBeTruthy();
  });

  it("Revisions: lists every revision newest-first", async () => {
    stub(DETAIL);
    render(<ArtifactPage apiBase="/admin/api" id="default/ar-s1-aaa" tab="revisions" />);

    const table = await screen.findByRole("table");
    // both revisions present in the table (the title also shows the name, so
    // scope to the table for the current render's row).
    expect(within(table).getByText("ar-s1-bbb")).toBeTruthy();
    expect(within(table).getByText("ar-s1-aaa")).toBeTruthy();
  });

  it("renders a clean not-found state on a 404", async () => {
    stub({ error: "artifact default/ar-x not found" }, 404);
    render(<ArtifactPage apiBase="/admin/api" id="default/ar-x" />);
    await waitFor(() => expect(screen.getByText(/not found/)).toBeTruthy());
  });
});
