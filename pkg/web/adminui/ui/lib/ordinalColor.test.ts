import { describe, expect, it } from "vitest";
import { ORDINAL_PALETTE, ordinalColor, ordinalColorMap } from "./ordinalColor";

describe("ordinalColorMap", () => {
  it("is stable: same distinct values → same map regardless of input order/dupes", () => {
    const a = ordinalColorMap(["kind_b", "kind_a", "kind_a", "kind_c"]);
    const b = ordinalColorMap(["kind_c", "kind_a", "kind_b"]);
    expect([...a.entries()]).toEqual([...b.entries()]);
    // sorted assignment: first palette color goes to the lex/first value.
    expect(a.get("kind_a")).toBe(ORDINAL_PALETTE[0]);
    expect(a.get("kind_b")).toBe(ORDINAL_PALETTE[1]);
    expect(a.get("kind_c")).toBe(ORDINAL_PALETTE[2]);
  });

  it("gives distinct values distinct colors up to the palette length", () => {
    const vals = Array.from({ length: ORDINAL_PALETTE.length }, (_, i) => `v${String(i).padStart(2, "0")}`);
    const m = ordinalColorMap(vals);
    expect(new Set(m.values()).size).toBe(ORDINAL_PALETTE.length);
  });

  it("wraps the palette once distinct values exceed it", () => {
    const n = ORDINAL_PALETTE.length;
    const vals = Array.from({ length: n + 1 }, (_, i) => `v${String(i).padStart(2, "0")}`);
    const m = ordinalColorMap(vals);
    // the (n+1)-th distinct value reuses the first palette color.
    expect(m.get(vals[n])).toBe(ORDINAL_PALETTE[0]);
  });

  it("drops empty values (no category has an empty key)", () => {
    const m = ordinalColorMap(["", "x", ""]);
    expect(m.has("")).toBe(false);
    expect(m.get("x")).toBe(ORDINAL_PALETTE[0]);
  });
});

describe("ordinalColor", () => {
  it("is deterministic and returns a palette color for a value", () => {
    expect(ordinalColor("slack")).toBe(ordinalColor("slack"));
    expect(ORDINAL_PALETTE).toContain(ordinalColor("slack"));
  });

  it("is stable per value regardless of any surrounding set (unlike ordinalColorMap)", () => {
    // ordinalColorMap("slack") depends on the other values in the list; ordinalColor
    // hashes the single value, so it's the SAME color everywhere in the app.
    const first = ordinalColor("oauth");
    const again = ordinalColor("oauth");
    expect(first).toBe(again);
    expect(first).toBeTruthy();
  });

  it("returns undefined for the empty value (no category)", () => {
    expect(ordinalColor("")).toBeUndefined();
  });
});
