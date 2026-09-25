import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { SourcePage } from "./SourcePage";
import type { ResourceDetail } from "../../lib/api";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});
beforeEach(() => window.history.pushState({}, "", "/admin/source/default/team-skills"));

function stubFetch(body: unknown, status = 200) {
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(body), { status })));
}

describe("SourcePage", () => {
  it("Overview: renders the curated repo/ref/discovered-skills fields", async () => {
    const detail: ResourceDetail = {
      name: "team-skills",
      namespace: "default",
      scope: "namespaced",
      status: "Ready",
      description: "https://github.com/acme/team-skills",
      manageCmd: "kubectl edit skillsource team-skills -n default",
      sections: [
        {
          id: "overview",
          label: "Overview",
          kind: "fields",
          fields: [
            { label: "Repo URL", value: "https://github.com/acme/team-skills" },
            { label: "Ref", value: "main" },
            { label: "Discovered skills", value: "3" },
          ],
        },
      ],
    };
    stubFetch(detail);

    render(<SourcePage apiBase="/admin/api" id="default/team-skills" />);

    await waitFor(() => expect(screen.getByText("main")).toBeTruthy());
    expect(screen.getByText("3")).toBeTruthy();
    // The repo URL appears twice: the header description (plain text) and the
    // Repo URL field. The field now renders as a safe external anchor — fixing
    // the regression where an http(s) source URL was inert plain text.
    const repos = screen.getAllByText("https://github.com/acme/team-skills");
    expect(repos.length).toBe(2);
    const anchor = repos.map((n) => n.closest("a")).find((a): a is HTMLAnchorElement => a != null);
    expect(anchor).toBeTruthy();
    expect(anchor!.getAttribute("href")).toBe("https://github.com/acme/team-skills");
    expect(anchor!.getAttribute("target")).toBe("_blank");
    expect(anchor!.getAttribute("rel")).toBe("noopener noreferrer nofollow");
  });
});
