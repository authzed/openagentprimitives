import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { ProviderPage } from "./ProviderPage";
import type { ResourceDetail, ResourceRow } from "../../lib/api";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});
beforeEach(() => window.history.pushState({}, "", "/admin/provider/okta"));

// routeFetch dispatches a stubbed response per URL suffix. Providers are
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

const PROVIDERS_LIST: ResourceRow[] = [{ name: "okta", scope: "cluster", status: "Valid" }];

describe("ProviderPage", () => {
  it("resolves the bare-name provider and renders its detail via /config/providers", async () => {
    const detail: ResourceDetail = {
      name: "okta",
      scope: "cluster",
      status: "Valid",
      description: "OIDC login provider",
      manageCmd: "kubectl edit clusteridentityprovider okta",
      sections: [
        {
          id: "overview",
          label: "Overview",
          kind: "fields",
          fields: [
            { label: "Kind", value: "oidc" },
            { label: "Issuer", value: "https://okta.example" },
          ],
        },
      ],
    };
    routeFetch({ "/config/providers/okta": detail, "/config/providers": PROVIDERS_LIST });

    render(<ProviderPage apiBase="/admin/api" id="okta" />);

    // Detail loaded from the providers projector (issuer field surfaced).
    await waitFor(() => expect(screen.getByText("https://okta.example")).toBeTruthy());
    expect(screen.getByText("OIDC login provider")).toBeTruthy();
    // Kind renders as a color-coded KindBadge.
    expect(screen.getByText("oidc")).toBeTruthy();
  });
});
