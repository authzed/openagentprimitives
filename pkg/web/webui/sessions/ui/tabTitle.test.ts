import { describe, it, expect } from "vitest";
import { formatTabTitle } from "./tabTitle";

describe("formatTabTitle", () => {
  it("is the bare base when no conversation is open", () => {
    expect(formatTabTitle(null, 0)).toBe("oap sessions");
  });
  it("prefixes the active conversation title before the base", () => {
    expect(formatTabTitle("deploy questions", 0)).toBe("deploy questions · oap sessions");
  });
  it("adds an unread badge when there are away-replies", () => {
    expect(formatTabTitle("deploy questions", 2)).toBe("(2) deploy questions · oap sessions");
    expect(formatTabTitle(null, 3)).toBe("(3) oap sessions");
  });
  it("treats a non-positive unread as no badge", () => {
    expect(formatTabTitle("x", 0)).toBe("x · oap sessions");
    expect(formatTabTitle("x", -1)).toBe("x · oap sessions");
  });
});
