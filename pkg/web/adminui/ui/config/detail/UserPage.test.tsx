import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { UserPage } from "./UserPage";
import type { ResourceDetail, ResourceRow } from "../../lib/api";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});
beforeEach(() => window.history.pushState({}, "", "/admin/user/Alice"));

// routeFetch dispatches a stubbed response per URL suffix. Users are
// cluster-scoped, so the detail route id is a BARE name — ConfigDetail lists the
// resource to recover the row before fetching the detail; both endpoints stub here.
function routeFetch(routes: Record<string, unknown>) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) => {
      for (const [suffix, body] of Object.entries(routes)) {
        if (url.endsWith(suffix)) return new Response(JSON.stringify(body), { status: 200 });
      }
      return new Response(JSON.stringify({ error: `no stub for ${url}` }), { status: 404 });
    }),
  );
}

const USERS_LIST: ResourceRow[] = [{ name: "Alice", scope: "cluster", status: "Valid" }];

// The users detail carries no manageCmd (UserIdentity CRs are owned by the
// identity setup flow, not hand-edited) — so the Overview must NOT render a
// Manage / CopyCmd block.
describe("UserPage", () => {
  it("Overview: renders curated fields and omits the Manage block when manageCmd is empty", async () => {
    const detail: ResourceDetail = {
      name: "Alice",
      scope: "cluster",
      status: "Valid",
      description: "alice@example.com",
      sections: [
        {
          id: "overview",
          label: "Overview",
          kind: "fields",
          fields: [
            { label: "Subject", value: "alice@example.com" },
            { label: "Display name", value: "Alice" },
          ],
        },
        {
          id: "credentials",
          label: "Credentials",
          kind: "list",
          items: [{ title: "github-token", badges: [{ key: "type", value: "github_pat" }] }],
        },
      ],
    };
    routeFetch({ "/config/users/Alice": detail, "/config/users": USERS_LIST });

    render(<UserPage apiBase="/admin/api" id="Alice" />);

    await waitFor(() => expect(screen.getByText("Subject")).toBeTruthy());
    // curated overview field surfaced (the subject appears as description + field)
    expect(screen.getAllByText("alice@example.com").length).toBeGreaterThan(0);
    // no Manage block
    expect(screen.queryByText("Manage")).toBeNull();
    // the Credentials section still becomes a tab
    expect(screen.getByRole("tab", { name: "Credentials" })).toBeTruthy();
  });

  it("surfaces the DECODED human email as the page subtitle when the description is an encoded subject", async () => {
    // description is the canonical subject user:base64url("alice@example.com").
    const detail: ResourceDetail = {
      name: "Alice",
      scope: "cluster",
      status: "Valid",
      description: "user:YWxpY2VAZXhhbXBsZS5jb20",
      sections: [
        {
          id: "overview",
          label: "Overview",
          kind: "fields",
          fields: [{ label: "Subject", value: "user:YWxpY2VAZXhhbXBsZS5jb20" }],
        },
      ],
    };
    routeFetch({ "/config/users/Alice": detail, "/config/users": USERS_LIST });

    render(<UserPage apiBase="/admin/api" id="Alice" />);

    // The decoded email is shown (as the prominent subtitle) even though the raw
    // subject the projector emitted is the opaque base64url form.
    await waitFor(() => expect(screen.getByText("alice@example.com")).toBeTruthy());
  });
});
