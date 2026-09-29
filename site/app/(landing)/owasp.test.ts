import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { anchorIds } from "@/lib/links";
import { OWASP } from "./owasp";

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
