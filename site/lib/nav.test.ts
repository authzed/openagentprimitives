import { describe, expect, it } from "vitest";
import { buildNav, normalizeGuide, sortGuides, type Guide } from "./nav";

const g = (slug: string, order: number, section = "Guides", group = "", title = slug): Guide => ({
  slug, title, section, group, order,
});

describe("normalizeGuide", () => {
  it("missing meta: title=slug, section=Guides, group='', order=100", () => {
    expect(normalizeGuide("x", undefined)).toEqual({
      slug: "x", title: "x", section: "Guides", group: "", order: 100, description: undefined,
    });
  });
  it("partial meta: fills only the absent fields", () => {
    expect(normalizeGuide("x", { title: "X", order: 5 })).toMatchObject({
      title: "X", section: "Guides", group: "", order: 5,
    });
  });
});

describe("sortGuides", () => {
  it("orders by `order`, then title on ties, without mutating input", () => {
    const input = [g("b", 2), g("zeta", 1), g("alpha", 1)];
    expect(sortGuides(input).map((x) => x.slug)).toEqual(["alpha", "zeta", "b"]);
    expect(input.map((x) => x.slug)).toEqual(["b", "zeta", "alpha"]);
  });
});

describe("buildNav", () => {
  it("sections appear in first-seen order of the sorted list", () => {
    const nav = buildNav([g("a", 1, "Guides"), g("b", 2, "Reference"), g("c", 3, "Guides")]);
    expect(nav.map((s) => s.section)).toEqual(["Guides", "Reference"]);
  });
  it("an ungrouped guide is its own block at its own position; a group anchors at its lowest-order child", () => {
    const nav = buildNav([
      g("intro", 1),
      g("c1", 2, "Guides", "Concepts"),
      g("mid", 3),
      g("c2", 4, "Guides", "Concepts"),
    ]);
    expect(nav[0].blocks.map((b) => [b.group, b.guides.map((x) => x.slug)])).toEqual([
      ["", ["intro"]],
      ["Concepts", ["c1", "c2"]],
      ["", ["mid"]],
    ]);
  });
});
