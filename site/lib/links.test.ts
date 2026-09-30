import { describe, expect, it } from "vitest";
import { anchorIds, checkLinks, extractDocLinks, linkKind } from "./links";

describe("linkKind", () => {
  it.each([
    ["/docs/safe-tools", "docs"],
    ["/docs/owasp-top10#asi03", "docs"],
    ["/docs", "docs"],
    ["https://example.com", "external"],
    ["http://example.com", "external"],
    ["#asi03", "plain"],
    ["mailto:a@b.c", "plain"],
    ["/", "plain"],
    ["relative/path", "plain"],
    ["/docsx", "plain"],
  ] as const)("%s → %s", (href, want) => {
    expect(linkKind(href)).toBe(want);
  });
});

describe("extractDocLinks", () => {
  it("finds markdown and JSX doc links, with and without anchors", () => {
    const src =
      'See [a](/docs/a) and [b](/docs/b#x). <a href="/docs/c">c</a> [old](#/d)';
    expect(extractDocLinks(src)).toEqual([
      "/docs/a",
      "/docs/b#x",
      "/docs/c",
      "#/d",
    ]);
  });
  it("skips external links, same-page anchors, and mailto", () => {
    expect(
      extractDocLinks("[e](https://x.io/docs/a) [h](#top) [m](mailto:a@b.c)"),
    ).toEqual([]);
  });
  it("ignores links inside fenced code blocks and inline code", () => {
    const src =
      "```\n[a](/docs/nope)\n```\nand `[b](#/nope)` but [c](/docs/yes)";
    expect(extractDocLinks(src)).toEqual(["/docs/yes"]);
  });
  it("finds object-literal href fields", () => {
    expect(extractDocLinks('{ href: "/docs/a" }')).toEqual(["/docs/a"]);
    expect(extractDocLinks("{ href: '/docs/b' }")).toEqual(["/docs/b"]);
  });
});

describe("anchorIds", () => {
  it("collects id attributes", () => {
    expect(anchorIds('<h3 id="asi01">x</h3> <span id="b" />')).toEqual(
      new Set(["asi01", "b"]),
    );
  });
});

describe("checkLinks", () => {
  const guides = new Map([
    ["a", new Set<string>()],
    ["owasp", new Set(["asi01"])],
  ]);
  it.each([
    ["/docs/a", null],
    ["/docs", null],
    ["/docs/owasp#asi01", null],
    ["/docs/missing", "no guide 'missing'"],
    ["/docs/owasp#asi99", "no id 'asi99' in 'owasp'"],
    ["#/a", "hash route; use /docs/a"],
  ])("%s → %s", (href, reason) => {
    const got = checkLinks([{ file: "f.mdx", src: `[x](${href})` }], guides);
    expect(got).toEqual(reason ? [{ file: "f.mdx", href, reason }] : []);
  });
});
