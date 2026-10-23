import { describe, expect, it } from "vitest";
import { normalizeGuide } from "./nav";
import { SITE_URL } from "./site";
import { guideGraph, landingGraph, plainText } from "./structured-data";

type Node = Record<string, unknown>;
const nodes = (g: Node) => g["@graph"] as Node[];
const byType = (g: Node, type: string) =>
  nodes(g).find((n) => n["@type"] === type);

describe("landingGraph", () => {
  it("declares the organization, website and project, cross-linked by @id", () => {
    const g = landingGraph();
    expect(g["@context"]).toBe("https://schema.org");
    const org = byType(g, "Organization");
    const site = byType(g, "WebSite");
    const sw = byType(g, "SoftwareSourceCode");
    expect(org?.name).toBe("AuthZed");
    expect(site?.url).toBe(`${SITE_URL}/`);
    expect(site?.publisher).toEqual({ "@id": org?.["@id"] });
    expect(sw?.codeRepository).toMatch(/^https:\/\/github\.com\//);
    expect(sw?.author).toEqual({ "@id": org?.["@id"] });
  });
});

describe("guideGraph", () => {
  const guide = normalizeGuide("plan-gating", {
    title: "Plan gating",
    section: "Guides",
    group: "Security",
  });
  const g = guideGraph(guide, "Desc.", "/docs/plan-gating/opengraph-image");

  it("describes the page as a TechArticle with an absolute image", () => {
    const article = byType(g, "TechArticle");
    expect(article).toMatchObject({
      headline: "Plan gating",
      description: "Desc.",
      url: `${SITE_URL}/docs/plan-gating`,
      image: `${SITE_URL}/docs/plan-gating/opengraph-image`,
      articleSection: "Security",
    });
  });

  it("includes every node it references by @id", () => {
    const ids = new Set(nodes(g).map((n) => n["@id"]));
    const refs = JSON.stringify(g).matchAll(/\{"@id":"([^"]+)"\}/g);
    for (const [, id] of refs) expect(ids).toContain(id);
  });

  it("gives every breadcrumb a URL, ending at the page", () => {
    const crumbs = byType(g, "BreadcrumbList")?.itemListElement as Node[];
    expect(crumbs.map((c) => c.position)).toEqual([1, 2, 3]);
    expect(crumbs.every((c) => typeof c.item === "string")).toBe(true);
    expect(crumbs.at(-1)).toMatchObject({
      name: "Plan gating",
      item: `${SITE_URL}/docs/plan-gating`,
    });
  });
});

describe("plainText", () => {
  it.each([
    ["code span", "a moving `latest` tag", "a moving latest tag"],
    [
      "emphasis",
      "an *external* and **bold** effect",
      "an external and bold effect",
    ],
    ["plain", "already plain", "already plain"],
  ])("%s: stripped to plain text", (_, input, want) => {
    expect(plainText(input)).toBe(want);
  });
});
