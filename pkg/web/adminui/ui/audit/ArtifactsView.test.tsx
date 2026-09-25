import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import * as React from "react";
import { ArtifactsView } from "./ArtifactsView";
import type { ArtifactRow } from "../lib/api";

afterEach(cleanup);

const rows: ArtifactRow[] = [
  {
    name: "ar-s1-aaa", namespace: "default", artifactId: "art-1", session: "default/s1", kind: "html",
    phase: "Ready", mime: "text/html", size: 2048, revisions: 2, created: "2026-06-10T12:00:00Z",
  },
  {
    name: "ar-s2-ccc", namespace: "default", artifactId: "art-2", session: "default/s2", kind: "image",
    phase: "Failed", mime: "image/png", size: 0, revisions: 1, created: "2026-06-10T11:00:00Z",
  },
];

describe("ArtifactsView", () => {
  it("renders rows with kind, MIME, and color-coded phase pills", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(rows), { status: 200 })));
    render(<ArtifactsView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText("ar-s1-aaa")).toBeTruthy());
    expect(screen.getByText("Ready")).toBeTruthy();
    expect(screen.getByText("Failed")).toBeTruthy();
    expect(screen.getByText("text/html")).toBeTruthy();
    // revisions count rendered for the multi-render artifact
    expect(screen.getByText("2")).toBeTruthy();
  });

  it("links the artifact name to its detail page", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(rows), { status: 200 })));
    render(<ArtifactsView apiBase="/admin/api" />);
    const link = (await screen.findByText("ar-s1-aaa")) as HTMLAnchorElement;
    expect(link.getAttribute("href")).toBe("/admin/artifact/default/ar-s1-aaa");
  });

  it("deep-links the session cell to its audit logs via onDrill", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(rows), { status: 200 })));
    const onDrill = vi.fn();
    render(<ArtifactsView apiBase="/admin/api" onDrill={onDrill} />);
    await waitFor(() => expect(screen.getByText("ar-s1-aaa")).toBeTruthy());
    // The default namespace is hidden in the Session cell → visible text is "s1".
    fireEvent.click(screen.getByText("s1"));
    expect(onDrill).toHaveBeenCalledWith({ sessionNamespace: "default", sessionName: "s1" });
  });

  it("renders only the latest revision of a logical artifact in the primary color", async () => {
    // Two renders share a logical artifact (same namespace + artifactId); the
    // newer one links in primary, the older one muted.
    const grouped: ArtifactRow[] = [
      {
        name: "ar-new", namespace: "default", artifactId: "art-shared", session: "default/s1", kind: "html",
        phase: "Ready", mime: "text/html", size: 100, revisions: 2, created: "2026-06-10T12:00:00Z",
      },
      {
        name: "ar-old", namespace: "default", artifactId: "art-shared", session: "default/s1", kind: "html",
        phase: "Ready", mime: "text/html", size: 90, revisions: 2, created: "2026-06-10T11:00:00Z",
      },
    ];
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(grouped), { status: 200 })));
    render(<ArtifactsView apiBase="/admin/api" />);

    const newer = (await screen.findByText("ar-new")).closest("a") as HTMLAnchorElement;
    const older = (await screen.findByText("ar-old")).closest("a") as HTMLAnchorElement;
    expect(newer.className).toContain("text-link");
    expect(older.className).toContain("text-muted-foreground");
    expect(older.className).not.toContain("text-link");
  });

  it("shows an empty state when there are no artifacts", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("[]", { status: 200 })));
    render(<ArtifactsView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText(/No artifacts rendered yet/)).toBeTruthy());
  });

  it("shows an error state when the fetch fails", async () => {
    vi.stubGlobal("fetch", vi.fn(async () =>
      new Response(JSON.stringify({ error: "boom" }), { status: 500 })));
    render(<ArtifactsView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText(/Could not load artifacts: boom/)).toBeTruthy());
  });
});
