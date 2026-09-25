import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { UsersView } from "./UsersView";

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

// The users projector now emits an empty manageCmd, so the generic table must
// drop the Manage column entirely rather than render an all-blank one.
const users = [
  {
    name: "alice", namespace: "default", scope: "namespaced", status: "Valid",
    badges: [{ key: "subject", value: "service:hubspot-bot" }],
    counts: [{ label: "availableCredentials", value: 2 }, { label: "resolved", value: 2 }],
    manageCmd: "",
  },
  {
    name: "bob", namespace: "default", scope: "namespaced", status: "Valid",
    badges: [{ key: "subject", value: "service:zendesk-bot" }],
    counts: [{ label: "availableCredentials", value: 1 }, { label: "resolved", value: 0 }],
    manageCmd: "",
  },
];

describe("UsersView", () => {
  it("renders no Manage column when every row's manageCmd is empty", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(users), { status: 200 })));
    render(<UsersView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText("alice")).toBeTruthy());

    // Other headers still present; Manage is gone.
    expect(screen.getByRole("columnheader", { name: "Status" })).toBeTruthy();
    expect(screen.queryByRole("columnheader", { name: "Manage" })).toBeNull();
  });
});
