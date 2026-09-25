import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import * as React from "react";
import { SourcesView } from "./SourcesView";

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

function stub(rows: unknown) {
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(rows), { status: 200 })));
}

describe("SourcesView", () => {
  it("renders an http(s) repo as a hardened external link", async () => {
    stub([
      {
        name: "team-skills", namespace: "default", scope: "namespaced", status: "Ready",
        badges: [
          { key: "kind", value: "skillsource" },
          { key: "repo", value: "https://github.com/acme/team-skills" },
        ],
        manageCmd: "kubectl -n default edit skillsource team-skills",
      },
    ]);
    render(<SourcesView apiBase="/admin/api" />);

    const link = (await screen.findByText("https://github.com/acme/team-skills")) as HTMLAnchorElement;
    expect(link.tagName).toBe("A");
    expect(link.getAttribute("rel")).toBe("noopener noreferrer nofollow");
  });

  it("renders a javascript: repo as plain text — NO clickable anchor (XSS guard)", async () => {
    const evil = "javascript:alert(1)";
    stub([
      {
        name: "evil-src", namespace: "default", scope: "namespaced", status: "Ready",
        badges: [
          { key: "kind", value: "skillsource" },
          { key: "repo", value: evil },
        ],
        manageCmd: "kubectl -n default edit skillsource evil-src",
      },
    ]);
    render(<SourcesView apiBase="/admin/api" />);

    const node = await screen.findByText(evil);
    expect(node.tagName).toBe("CODE");
    expect(screen.queryByRole("link", { name: evil })).toBeNull();
  });
});
