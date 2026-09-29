import { describe, expect, it, vi } from "vitest";
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
  it("returns null and logs the cause when the bundle is missing", async () => {
    const log = vi.fn();
    const err = new Error("404");
    expect(await loadPagefind(() => Promise.reject(err), log)).toBeNull();
    expect(log).toHaveBeenCalledWith(expect.any(String), err);
  });
  it("returns the module when the import succeeds", async () => {
    const pf = { search: async () => ({ results: [] }) };
    const log = vi.fn();
    expect(await loadPagefind(async () => pf, log)).toBe(pf);
    expect(log).not.toHaveBeenCalled();
  });
});
