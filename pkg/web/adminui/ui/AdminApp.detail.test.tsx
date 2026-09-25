import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { AdminApp } from "./AdminApp";
import { ALL_ENTITY_KINDS } from "./lib/router";
import type { ResourceDetail } from "./lib/api";

// PLACEHOLDER is the exact copy DetailPlaceholder renders. Asserting on it is
// the whole point of this file: detailContent's `default:` arm means a MISSING
// case renders a coherent-looking page instead of throwing, so a route with no
// page looks fine to every test that only checks "something rendered".
const PLACEHOLDER = "Detail page lands in a later batch.";

const DIRECTORY_DETAIL: ResourceDetail = {
  name: "demo-github",
  namespace: "default",
  scope: "namespaced",
  status: "Ready",
  description: "Directory sync (github)",
  manageCmd: "oap directory configure",
  sections: [
    {
      id: "overview",
      label: "Overview",
      kind: "fields",
      fields: [
        { label: "Kind", value: "github" },
        { label: "Interval", value: "15m0s" },
      ],
    },
    {
      id: "sync",
      label: "Sync",
      kind: "fields",
      fields: [{ label: "Join misses", value: "7" }],
    },
  ],
};

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  window.history.pushState({}, "", "/admin");
});

// stubFetch answers every request with body/status. The detail pages differ in
// which endpoints they hit; this test only cares that the route reaches a real
// page, so one answer for all of them is enough.
function stubFetch(body: unknown, status = 200) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(JSON.stringify(body), { status })),
  );
}

describe("AdminApp detail routing", () => {
  // C1: DirectoryView links every row to /admin/directory/<ns>/<name>, but
  // detailContent had no "directory" case, so every click landed on
  // DetailPlaceholder and the whole directoryDetailProjector was served and
  // never requested.
  it("directory detail: renders the real RelationshipSource page, not the placeholder", async () => {
    window.history.pushState({}, "", "/admin/directory/default/demo-github");
    stubFetch(DIRECTORY_DETAIL);

    render(<AdminApp apiBase="/admin/api" />);

    // The real page renders the projector's own fields…
    await waitFor(() => expect(screen.getByText("github")).toBeTruthy());
    expect(screen.getByText("15m0s")).toBeTruthy();
    // …and the placeholder is nowhere on the page. Both halves matter: the
    // first alone would pass against any page that happened to render text,
    // and the second alone would pass against a blank screen.
    expect(screen.queryByText(PLACEHOLDER)).toBeNull();
  });

  // The class guard, not just the instance. Every EntityKind is reachable from
  // some list view's EntityLink, so every one needs a case in detailContent;
  // ALL_ENTITY_KINDS is derived from the router's own table so adding a kind
  // adds a case to this loop automatically.
  //
  // A 404 answer is deliberate: it drives each page to its own "not found"
  // state without needing a per-entity fixture. The placeholder does NOT fetch
  // at all, so it renders its copy regardless — which is exactly what makes
  // "no placeholder text" a real signal here.
  it.each(ALL_ENTITY_KINDS)("%s detail: has a real page (never DetailPlaceholder)", async (entity) => {
    window.history.pushState({}, "", `/admin/${entity}/default/whatever`);
    stubFetch({ error: "not found" }, 404);

    render(<AdminApp apiBase="/admin/api" />);

    await waitFor(() => expect(screen.queryByText(PLACEHOLDER)).toBeNull());
  });
});
