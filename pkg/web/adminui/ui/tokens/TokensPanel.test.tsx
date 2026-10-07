import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { TokensPanel } from "./TokensPanel";
import type { AccessTokenRow } from "../lib/api";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

// jsonResponse builds a fetch Response the way admind's handlers do (mirrors
// WorkshopsView.test.tsx's own helper).
function jsonResponse(body: unknown, status: number) {
  return new Response(JSON.stringify(body), { status });
}

// row builds one AccessTokenRow fixture, overridable per test.
function row(overrides: Partial<AccessTokenRow> = {}): AccessTokenRow {
  return {
    // user:alice@example.com, base64url — decodeSubject round-trips this back
    // to the email once TokensPanel prefixes it with "user:".
    name: "at-full0000001",
    owner: "YWxpY2VAZXhhbXBsZS5jb20",
    clientName: "demo-client",
    role: "full",
    scopeClasses: [],
    unfiltered: true,
    createdAt: "2026-06-30T12:00:00Z",
    expiresAt: "2026-07-30T12:00:00Z",
    lastUsedAt: "2026-06-30T13:00:00Z",
    revoked: false,
    ...overrides,
  };
}

describe("TokensPanel", () => {
  it("lists tokens with the decoded owner, role, and scope — a revoked row shows a badge instead of Revoke", async () => {
    const rows: AccessTokenRow[] = [
      row(),
      row({
        name: "at-revoked0001",
        owner: "Ym9iQGV4YW1wbGUuY29t",
        role: "unknown",
        unfiltered: false,
        scopeClasses: undefined,
        revoked: true,
        lastUsedAt: undefined,
      }),
    ];
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ tokens: rows }, 200)));

    render(<TokensPanel apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText("alice@example.com")).toBeTruthy());

    expect(screen.getByText("bob@example.com")).toBeTruthy();
    expect(screen.getByText("full")).toBeTruthy();
    expect(screen.getByText("all classes")).toBeTruthy();
    expect(screen.getByText("never")).toBeTruthy(); // the revoked row's absent lastUsedAt

    // The healthy row gets a Revoke button; the revoked row gets a badge, not a button.
    expect(screen.getByRole("button", { name: "Revoke" })).toBeTruthy();
    expect(screen.getByText("Revoked")).toBeTruthy();
  });

  it("asks for confirmation before revoking, then POSTs and refetches", async () => {
    const initial = [row()];
    const afterRevoke = [row({ revoked: true, role: "unknown" })];
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse({ tokens: initial }, 200))
      .mockResolvedValueOnce(jsonResponse({ ok: true }, 200))
      .mockResolvedValueOnce(jsonResponse({ tokens: afterRevoke }, 200));
    vi.stubGlobal("fetch", fetchMock);
    vi.spyOn(window, "confirm").mockReturnValue(true);

    render(<TokensPanel apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByRole("button", { name: "Revoke" })).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Revoke" }));

    await waitFor(() => expect(screen.getByText("Revoked")).toBeTruthy());

    expect(window.confirm).toHaveBeenCalled();
    const revokeCall = fetchMock.mock.calls.find(([url]) => String(url).endsWith("/tokens/revoke"));
    expect(revokeCall).toBeTruthy();
    expect(revokeCall![1]).toMatchObject({ method: "POST" });
    expect(JSON.parse((revokeCall![1] as RequestInit).body as string)).toEqual({ name: row().name });
  });

  it("does nothing when the confirmation is declined", async () => {
    const fetchMock = vi.fn().mockResolvedValueOnce(jsonResponse({ tokens: [row()] }, 200));
    vi.stubGlobal("fetch", fetchMock);
    vi.spyOn(window, "confirm").mockReturnValue(false);

    render(<TokensPanel apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByRole("button", { name: "Revoke" })).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Revoke" }));

    await waitFor(() => expect(window.confirm).toHaveBeenCalled());
    // Only the initial list load — no revoke POST was ever issued.
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("shows an empty state with no tokens, and a load error as an alert", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ tokens: [] }, 200)));
    render(<TokensPanel apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText("No access tokens yet.")).toBeTruthy());

    cleanup();
    vi.stubGlobal("fetch", vi.fn(async () => jsonResponse({ error: "boom" }, 500)));
    render(<TokensPanel apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText(/Could not load access tokens: boom/)).toBeTruthy());
  });
});
