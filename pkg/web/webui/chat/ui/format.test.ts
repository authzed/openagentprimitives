import { describe, it, expect } from "vitest";
import { formatTokens, formatElapsed, progressCaption } from "./format";

describe("formatTokens", () => {
  it("renders counts compactly with SI suffixes", () => {
    expect(formatTokens(0)).toBe("0");
    expect(formatTokens(999)).toBe("999");
    expect(formatTokens(1000)).toBe("1K");
    expect(formatTokens(1200)).toBe("1.2K");
    expect(formatTokens(47200)).toBe("47.2K");
    expect(formatTokens(1_500_000)).toBe("1.5M");
  });
  it("clamps negatives to zero", () => {
    expect(formatTokens(-5)).toBe("0");
  });
});

describe("formatElapsed", () => {
  it("renders seconds and minutes", () => {
    expect(formatElapsed(0)).toBe("0s");
    expect(formatElapsed(34)).toBe("34s");
    expect(formatElapsed(60)).toBe("1m");
    expect(formatElapsed(80)).toBe("1m20s");
    expect(formatElapsed(125)).toBe("2m5s");
  });
});

describe("progressCaption", () => {
  it("builds the working status line from a snapshot", () => {
    expect(progressCaption({ elapsedSeconds: 34, inputTokens: 1200, outputTokens: 6400 })).toBe(
      "working · 34s · 1.2K in · 6.4K out",
    );
  });
  it("collapses to bare 'working' when no snapshot has landed yet", () => {
    expect(progressCaption(null)).toBe("working");
  });
});
