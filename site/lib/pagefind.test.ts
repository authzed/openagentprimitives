import { describe, expect, it } from "vitest";
import { loadPagefind, toPath } from "./pagefind";

describe("toPath", () => {
  it.each([
    ["/docs/safe-tools.html", "/docs/safe-tools"],
    ["/docs/owasp-top10.html#asi03", "/docs/owasp-top10#asi03"],
    ["/docs/safe-tools", "/docs/safe-tools"],
    ["/index.html", "/"],
  ])("%s → %s", (url, want) => expect(toPath(url)).toBe(want));
});

describe("loadPagefind", () => {
  it("returns null when the bundle is missing (next dev)", async () => {
    expect(await loadPagefind(() => Promise.reject(new Error("404")))).toBeNull();
  });
  it("returns the module when the import succeeds", async () => {
    const pf = { search: async () => ({ results: [] }) };
    expect(await loadPagefind(async () => pf)).toBe(pf);
  });
});
