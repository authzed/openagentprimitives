import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import * as React from "react";
import { AccessView } from "./AccessView";

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

describe("AccessView", () => {
  it("lists platform admins, the grant command, and a graceful unavailable-stats panel", async () => {
    vi.stubGlobal("fetch", vi.fn(async () =>
      new Response(JSON.stringify({
        admins: [
          // user:<base64url(alice@example.com)> — decoded back to the email.
          { subject: "user:YWxpY2VAZXhhbXBsZS5jb20", kind: "user" },
          { subject: "group:platform-ops#member", kind: "group" },
        ],
        grantCmd: "oap platform grant-admin <email>",
        stats: { schemaDefs: null, relationships: null, available: false },
      }), { status: 200 })));

    render(<AccessView apiBase="/admin/api" />);

    // The canonical user subject is decoded to its email; the plain group name
    // (not an encoded email) is shown raw.
    await waitFor(() => expect(screen.getByText("alice@example.com")).toBeTruthy());
    expect(screen.getByText("group:platform-ops#member")).toBeTruthy();
    expect(screen.getByText("oap platform grant-admin <email>")).toBeTruthy();
    expect(screen.getByLabelText("Copy command")).toBeTruthy();
    // available === false → unavailable panel, never an error
    expect(screen.getByText(/Stats unavailable/)).toBeTruthy();
  });

  it("shows live stats when the server reports them available", async () => {
    vi.stubGlobal("fetch", vi.fn(async () =>
      new Response(JSON.stringify({
        admins: [],
        grantCmd: "oap platform grant-admin <email>",
        stats: { schemaDefs: 12, relationships: 1284, available: true },
      }), { status: 200 })));

    render(<AccessView apiBase="/admin/api" />);

    await waitFor(() => expect(screen.getByText("12")).toBeTruthy());
    expect(screen.getByText("1,284")).toBeTruthy();
    expect(screen.getByText(/No platform admins granted yet/)).toBeTruthy();
  });
});
