import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { anchorIds } from "@/lib/links";
import { OWASP, coverageSummary } from "./owasp";

// The landing table builds each row's link as /docs/owasp-top10#<id>, which
// the static link check cannot follow. Pin it here instead.
describe("landing OWASP links", () => {
  const ids = anchorIds(
    readFileSync(
      fileURLToPath(
        new URL("../../content/docs/owasp-top10.mdx", import.meta.url),
      ),
      "utf8",
    ),
  );
  it.each(OWASP.map(([id]) => id))("%s has an anchor in owasp-top10", (id) => {
    expect(ids.has(id.toLowerCase())).toBe(true);
  });
});

describe("coverageSummary", () => {
  it("counts the landing table's rows", () => {
    const counts = { substantial: 0, partial: 0, na: 0 };
    for (const [, , level] of OWASP) counts[level]++;
    expect(coverageSummary(OWASP)).toBe(
      [
        counts.substantial && `${counts.substantial} substantial`,
        counts.partial && `${counts.partial} partial`,
        counts.na && `${counts.na} architectural N/A`,
      ]
        .filter(Boolean)
        .join(" · "),
    );
  });

  it.each([
    [
      "every level present, in fixed order",
      [
        ["A", "a", "partial", ""],
        ["B", "b", "na", ""],
        ["C", "c", "substantial", ""],
        ["D", "d", "partial", ""],
      ],
      "1 substantial · 2 partial · 1 architectural N/A",
    ],
    [
      "a level with no rows is left out",
      [
        ["A", "a", "substantial", ""],
        ["B", "b", "partial", ""],
      ],
      "1 substantial · 1 partial",
    ],
    ["no rows at all yields an empty caption", [], ""],
  ] as const)("%s", (_name, rows, want) =>
    expect(coverageSummary(rows)).toBe(want),
  );
});
