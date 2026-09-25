import { describe, it, expect } from "vitest";
import { parseNotice } from "./notice";

describe("parseNotice", () => {
  it("returns a success notice for ?notice=linked:github", () => {
    expect(parseNotice("?notice=linked%3Agithub")).toEqual({ kind: "success", text: "Linked github." });
  });
  it("returns a success notice for ?notice=revoked:github", () => {
    expect(parseNotice("?notice=revoked%3Agithub")).toEqual({ kind: "success", text: "Revoked github." });
  });
  it("renders linkedunverified as a warning", () => {
    const n = parseNotice("?notice=linkedunverified:github-token");
    expect(n).toEqual({ kind: "error", text: "Linked github-token — but the token could not be verified. It may not work." });
  });
  it("returns an error notice for ?error=...", () => {
    expect(parseNotice("?error=Could%20not%20save")).toEqual({ kind: "error", text: "Could not save" });
  });
  it("returns null when neither param is present", () => {
    expect(parseNotice("?d=a&sig=b")).toBeNull();
  });
});
