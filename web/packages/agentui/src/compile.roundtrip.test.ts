// This file only reads the shared testdata fixture off disk — no DOM is
// needed — and jsdom (the project default) rewrites `import.meta.url` to
// resolve against jsdom's mocked `self.location` instead of the real file
// path, breaking the `new URL(…, import.meta.url)` resolution below. Node env
// keeps import.meta.url as the real file:// URL (same reasoning as
// web/packages/design/src/components/ui/chart.test.tsx).
// @vitest-environment node
import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { compilePage } from "./compile";

// The TypeScript half of the JSX ⇄ nodes round trip (spec §12). The Go side
// (pkg/web/uicomponents/jsx_roundtrip_test.go) prints the fixture's nodes as
// the page and pins both files; this side compiles the page and requires the
// same nodes back. Both read the committed pair, so the two halves cannot
// drift apart unnoticed: a grammar change on either side fails here.
const fixtureDir = new URL("../../../../pkg/web/uicomponents/testdata/", import.meta.url);

describe("JSX ⇄ nodes round trip", () => {
  it("compiles the printer's page back to the printer's nodes", () => {
    const page = readFileSync(new URL("roundtrip.page.tsx", fixtureDir), "utf8");
    const nodes = JSON.parse(readFileSync(new URL("roundtrip.view.json", fixtureDir), "utf8"));
    expect(compilePage(page, "roundtrip.page.tsx")).toEqual(nodes);
  });
});
