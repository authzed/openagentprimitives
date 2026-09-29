import { describe, expect, it } from "vitest";
import { linkKind } from "./links";

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
