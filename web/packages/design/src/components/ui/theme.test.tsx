// @vitest-environment node
import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const tokens = readFileSync(
  fileURLToPath(new URL("../../tokens.css", import.meta.url)),
  "utf8",
);

function names(block: string): string[] {
  return [...block.matchAll(/--([a-z0-9-]+)\s*:/g)].map((m) => m[1]).sort();
}
const dark = tokens.slice(
  tokens.indexOf(":root {"),
  tokens.indexOf("}", tokens.indexOf(":root {")),
);
const lightStart = tokens.indexOf(':root[data-theme="light"] {');
const light = tokens.slice(lightStart, tokens.indexOf("}", lightStart));

describe("theme blocks", () => {
  // The whole reason the light block is a second copy of every token and not
  // an override of a few: a token missing from one block silently inherits the
  // OTHER theme's value, which is a dark rung on a light page (or the reverse)
  // with no error anywhere. Parity by name is the guard.
  it("define exactly the same token set", () => {
    expect(names(light)).toEqual(names(dark));
  });

  it("do not depend on prefers-color-scheme (the shell picks the theme, the OS does not)", () => {
    expect(tokens).not.toContain("prefers-color-scheme");
  });
});
