// The drift guard for the one first-party page: the committed page.view.json
// must be exactly what compiling page.tsx produces, and the page.view.sha256
// sidecar beside it must be the hash of that same source (the Go side reads
// the sidecar to refuse a stale view without a Node toolchain). `mage
// ui:compile` regenerates both; `mage web:check` fails on the same drift from
// the Go side.
//
// Resolves via `new URL(..., import.meta.url)`, which jsdom (the workspace's
// default test environment) rewrites against its mocked `self.location` —
// breaking file resolution. Node env keeps import.meta.url as the real
// file:// URL (same reasoning as compile.roundtrip.test.ts and
// web/packages/design/src/components/ui/chart.test.tsx).
// @vitest-environment node
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { compilePage } from "./compile";

const dir = new URL(
  "../../../../pkg/platform/builderbundle/src/ui/",
  import.meta.url,
);
const page = () => readFileSync(new URL("page.tsx", dir), "utf8");

describe("the builder page", () => {
  it("has a committed view that matches its source — run `mage ui:compile` if not", () => {
    const committed = JSON.parse(
      readFileSync(new URL("page.view.json", dir), "utf8"),
    );
    expect(compilePage(page(), "page.tsx")).toEqual(committed);
  });

  it("has a committed sha256 sidecar for that source — run `mage ui:compile` if not", () => {
    const sidecar = readFileSync(
      new URL("page.view.sha256", dir),
      "utf8",
    ).trim();
    expect(sidecar).toEqual(createHash("sha256").update(page()).digest("hex"));
  });
});
