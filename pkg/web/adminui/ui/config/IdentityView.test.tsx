import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import * as React from "react";
import { IdentityView } from "./IdentityView";

const identities = [
  {
    name: "gh-bot", namespace: "ns1", scope: "namespaced", status: "Valid",
    badges: [{ key: "cred", value: "static" }],
    counts: [{ label: "credentials", value: 1 }, { label: "resolved", value: 1 }],
    manageCmd: "kubectl edit agentidentity gh-bot -n ns1",
  },
  {
    name: "mcp-oauth", namespace: "ns1", scope: "namespaced", status: "Valid",
    badges: [{ key: "cred", value: "oauth" }],
    counts: [{ label: "credentials", value: 1 }, { label: "resolved", value: 0 }],
    manageCmd: "kubectl edit agentidentity mcp-oauth -n ns1",
  },
];

const providers = [
  {
    name: "okta", scope: "cluster", status: "Valid",
    badges: [{ key: "kind", value: "oidc" }, { key: "issuer", value: "https://okta.example" }],
    manageCmd: "kubectl edit clusteridentityprovider okta",
  },
];

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

function stub() {
  vi.stubGlobal("fetch", vi.fn(async (url: string) => {
    const u = String(url);
    if (u.includes("/config/providers")) return new Response(JSON.stringify(providers), { status: 200 });
    return new Response(JSON.stringify(identities), { status: 200 });
  }));
}

describe("IdentityView", () => {
  it("defaults to the Agent-identities tab and switches to Login providers", async () => {
    stub();
    render(<IdentityView apiBase="/admin/api" />);

    // Agent-identities tab is active by default.
    await waitFor(() => expect(screen.getByText("gh-bot")).toBeTruthy());
    expect(screen.queryByText("okta")).toBeNull();

    // Switch to the providers tab.
    fireEvent.click(screen.getByRole("tab", { name: "Login providers" }));
    await waitFor(() => expect(screen.getByText("okta")).toBeTruthy());
    expect(screen.queryByText("gh-bot")).toBeNull();

    // Provider rows are now linkable to their per-item detail page (cluster-
    // scoped → bare-name id).
    expect(screen.getByRole("link", { name: "okta" }).getAttribute("href")).toBe(
      "/admin/provider/okta",
    );

    // And back.
    fireEvent.click(screen.getByRole("tab", { name: "Agent identities" }));
    await waitFor(() => expect(screen.getByText("gh-bot")).toBeTruthy());
  });

  it("links an agent identity to its detail page and filters by credential type", async () => {
    stub();
    render(<IdentityView apiBase="/admin/api" />);
    await waitFor(() => expect(screen.getByText("gh-bot")).toBeTruthy());

    // Row name links to the identity detail page.
    expect(screen.getByRole("link", { name: "gh-bot" }).getAttribute("href")).toBe(
      "/admin/identity/ns1/gh-bot",
    );

    // The credential-type facet narrows to the oauth identity.
    fireEvent.click(screen.getByRole("button", { name: "oauth" }));
    expect(screen.getByText("mcp-oauth")).toBeTruthy();
    expect(screen.queryByText("gh-bot")).toBeNull();
  });
});
