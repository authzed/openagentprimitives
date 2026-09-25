import { describe, expect, it } from "vitest";
import { sessionRefSegments } from "./sessionRef";

// The refusal table is the guard's whole contract: anything that is not
// exactly two non-empty, non-dot segments must produce no address at all,
// because every caller splices the result straight into an iframe src.
describe("sessionRefSegments", () => {
  const refused: { name: string; ref: unknown }[] = [
    { name: "an empty string", ref: "" },
    { name: "one segment", ref: "a" },
    { name: "three segments", ref: "a/b/c" },
    { name: "a parent-relative first segment", ref: "../x" },
    { name: "an absolute path", ref: "/x" },
    { name: "a trailing slash", ref: "a/" },
    { name: "a dot segment", ref: "a/./b" },
    { name: "a non-string", ref: 42 },
  ];
  for (const tc of refused) {
    it(`refuses ${tc.name}, so no caller can build an address from it`, () => {
      expect(sessionRefSegments(tc.ref)).toBeNull();
    });
  }

  // Accepted unencoded: encoding is each caller's own step, after its prefix.
  it("accepts exactly two non-empty, non-dot segments and returns them unencoded", () => {
    expect(sessionRefSegments("ws-abc123456789/demo-haiku-1a2b3c4d")).toEqual([
      "ws-abc123456789",
      "demo-haiku-1a2b3c4d",
    ]);
  });
});
