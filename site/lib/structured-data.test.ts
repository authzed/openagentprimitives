import { describe, expect, it } from "vitest";
import { normalizeGuide } from "./nav";
import { SITE_URL } from "./site";
import { guideGraph, landingGraph, plainText, storyGraph } from "./structured-data";

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

describe("storyGraph", () => {
  const story = {
    title: "A story",
    description: "Desc.",
    path: "/stories/demo",
    keywords: ["a", "b"],
    published: "2026-10-22",
    image: "/opengraph-image",
  };
  const clip = {
    kind: "clip" as const,
    mp4: "/media/x.mp4",
    poster: "/media/x.png",
    caption: "A demo.",
    title: "X demo",
    uploadDate: "2026-10-22",
    transcript: ["One.", "Two."],
  };
  const faq = [{ q: "Why?", a: "Run `oap agent install`." }];

  it("dated article: dateModified falls back to published", () => {
    expect(byType(storyGraph(story, faq), "TechArticle")).toMatchObject({
      headline: "A story",
      url: `${SITE_URL}/stories/demo`,
      keywords: "a, b",
      datePublished: "2026-10-22",
      dateModified: "2026-10-22",
    });
  });

  it("FAQ answers are plain text: code spans stripped", () => {
    const entities = byType(storyGraph(story, faq), "FAQPage")?.mainEntity as Node[] | undefined;
    expect(entities?.[0]).toMatchObject({ name: "Why?", acceptedAnswer: { text: "Run oap agent install." } });
  });

  it("complete clip: VideoObject with absolute URLs and a joined transcript", () => {
    expect(byType(storyGraph(story, faq, clip), "VideoObject")).toMatchObject({
      thumbnailUrl: `${SITE_URL}/media/x.png`,
      contentUrl: `${SITE_URL}/media/x.mp4`,
      transcript: "One. Two.",
    });
  });

  it("clip missing a required field: no VideoObject rather than a partial one", () => {
    expect(byType(storyGraph(story, faq, { ...clip, uploadDate: undefined }), "VideoObject")).toBeUndefined();
  });

  it("includes every node it references by @id", () => {
    const g = storyGraph(story, faq, clip);
    const ids = new Set(nodes(g).map((n) => n["@id"]));
    for (const [, id] of JSON.stringify(g).matchAll(/\{"@id":"([^"]+)"\}/g)) expect(ids).toContain(id);
  });
});
