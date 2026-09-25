import { describe, it, expect } from "vitest";
import { relativeTime } from "./format";

describe("relativeTime", () => {
  const now = Date.parse("2026-07-04T12:00:00Z");
  it("renders compact relative labels", () => {
    expect(relativeTime("2026-07-04T11:59:50Z", now)).toBe("just now");
    expect(relativeTime("2026-07-04T11:55:00Z", now)).toBe("5m");
    expect(relativeTime("2026-07-04T09:00:00Z", now)).toBe("3h");
    expect(relativeTime("2026-07-01T12:00:00Z", now)).toBe("3d");
  });
  it("keeps 45-59s-old sessions as 'just now' (never the nonsensical '0m')", () => {
    expect(relativeTime("2026-07-04T11:59:15Z", now)).toBe("just now"); // 45s
    expect(relativeTime("2026-07-04T11:59:05Z", now)).toBe("just now"); // 55s
    expect(relativeTime("2026-07-04T11:59:00Z", now)).toBe("1m"); // 60s crosses to 1m
  });
  it("returns empty for a missing or unparseable timestamp", () => {
    expect(relativeTime("", now)).toBe("");
    expect(relativeTime("not-a-date", now)).toBe("");
  });
});
