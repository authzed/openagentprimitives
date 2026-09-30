import { describe, expect, it } from "vitest";
import { checkLedeTags } from "./lede";

describe("checkLedeTags", () => {
  it('flags a <p className="doc-lede">', () => {
    const src = '<p className="doc-lede">\n  Some intro text.\n</p>\n';
    expect(checkLedeTags([{ file: "a.mdx", src }])).toEqual([
      { file: "a.mdx" },
    ]);
  });

  it('passes a <div className="doc-lede">', () => {
    const src = '<div className="doc-lede">\n  Some intro text.\n</div>\n';
    expect(checkLedeTags([{ file: "a.mdx", src }])).toEqual([]);
  });

  it("ignores unrelated <p> tags", () => {
    const src = "<p>Just a paragraph, not a lede.</p>";
    expect(checkLedeTags([{ file: "a.mdx", src }])).toEqual([]);
  });

  it("reports one problem per offending file across multiple files", () => {
    const bad = '<p className="doc-lede">x</p>';
    const good = '<div className="doc-lede">x</div>';
    expect(
      checkLedeTags([
        { file: "bad.mdx", src: bad },
        { file: "good.mdx", src: good },
      ]),
    ).toEqual([{ file: "bad.mdx" }]);
  });
});
