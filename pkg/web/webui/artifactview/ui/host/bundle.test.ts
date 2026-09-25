import { describe, it, expect } from "vitest";
import { AnnotationStore } from "./bundle";
import type { Captured } from "./capture";

const cap = (text: string): Captured => ({
  target: "element", elementPath: "#x", fullPath: "#x", tagName: "div",
  cssClasses: [], elementText: text, selectedText: "", nearbyText: "",
  nearbyElements: [], computedStyles: {}, accessibility: { role: "", label: "" },
  boundingBox: { x: 0, y: 0, width: 0, height: 0 },
});

describe("AnnotationStore", () => {
  it("numbers annotations 1-based in add order", () => {
    const s = new AnnotationStore();
    const a = s.add(cap("a")); const b = s.add(cap("b"));
    expect([a.index, b.index]).toEqual([1, 2]);
    expect(s.count()).toBe(2);
  });
  it("keeps stable ids after a removal (no renumber — pins never need relabeling)", () => {
    const s = new AnnotationStore();
    s.add(cap("a")); s.add(cap("b")); s.add(cap("c"));
    s.remove(2);
    expect(s.list().map((x) => x.index)).toEqual([1, 3]); // 2 removed, 1 and 3 keep their ids
    expect(s.list().map((x) => x.elementText)).toEqual(["a", "c"]);
  });
  it("never reuses an id after a removal (no collision with a surviving annotation)", () => {
    const s = new AnnotationStore();
    s.add(cap("a")); s.add(cap("b")); s.add(cap("c")); // 1,2,3
    s.remove(2);
    const d = s.add(cap("d"));
    expect(d.index).toBe(4); // monotonic — not 2 (removed) and not 3 (still present)
    expect(s.list().map((x) => x.index)).toEqual([1, 3, 4]);
  });
  it("addExtra attaches more captured targets to one annotation (multi-component)", () => {
    const s = new AnnotationStore();
    const a = s.add(cap("primary"));
    s.addExtra(a.index, cap("second"));
    s.addExtra(a.index, cap("third"));
    const got = s.list()[0];
    expect(got.extra?.map((e) => e.elementText)).toEqual(["second", "third"]);
    // the bundle carries the extra targets
    expect(s.bundle("x").annotations[0].extra?.map((e) => e.elementText)).toEqual(["second", "third"]);
    // count is still one annotation
    expect(s.count()).toBe(1);
  });
  it("filled() counts only annotations whose comment is non-empty after trimming", () => {
    const s = new AnnotationStore();
    s.add(cap("a")); s.add(cap("b"));
    expect(s.filled()).toBe(0);
    s.setComment(1, "reword this");
    expect(s.filled()).toBe(1);
    s.setComment(1, "   "); // whitespace-only is not a real note
    expect(s.filled()).toBe(0);
  });
  it("comment is settable; chips are optional", () => {
    const s = new AnnotationStore();
    s.add(cap("a")); s.setComment(1, "reword this");
    s.setChip(1, "severity", "high");
    const bundle = s.bundle("artifact-9");
    expect(bundle.annotations[0].comment).toBe("reword this");
    expect(bundle.annotations[0].severity).toBe("high");
    expect(bundle.annotations[0].intent).toBeUndefined();
    expect(bundle.artifactId).toBe("artifact-9");
  });
});
