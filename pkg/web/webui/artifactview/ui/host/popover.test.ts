import { describe, it, expect, vi } from "vitest";
import { NotePopover } from "./popover";
import type { Annotation } from "./bundle";

function ann(overrides: Partial<Annotation> = {}): Annotation {
  return {
    target: "element", elementPath: "#cta", fullPath: "#cta", tagName: "button",
    cssClasses: [], elementText: "Start free trial", selectedText: "", nearbyText: "",
    nearbyElements: [], computedStyles: {}, accessibility: { role: "", label: "" },
    boundingBox: { x: 0, y: 0, width: 0, height: 0 },
    index: 1, comment: "", ...overrides,
  };
}

function harness() {
  const host = document.implementation.createHTMLDocument("host");
  const onComment = vi.fn();
  const onChip = vi.fn();
  const onDelete = vi.fn();
  const pop = new NotePopover(host, { onComment, onChip, onDelete });
  return { host, pop, onComment, onChip, onDelete };
}

describe("NotePopover", () => {
  it("open() renders a textarea prefilled with the annotation's comment", () => {
    const { host, pop } = harness();
    pop.open(ann({ index: 7, comment: "reword this" }));
    const ta = host.querySelector<HTMLTextAreaElement>(".ap-annot-pop textarea");
    expect(ta).not.toBeNull();
    expect(ta!.value).toBe("reword this");
    expect(host.querySelectorAll(".ap-annot-pop").length).toBe(1);
  });

  it("typing in the textarea reports the new comment for that annotation's id", () => {
    const { host, pop, onComment } = harness();
    pop.open(ann({ index: 7 }));
    const ta = host.querySelector<HTMLTextAreaElement>(".ap-annot-pop textarea")!;
    ta.value = "make this bigger";
    ta.dispatchEvent(new Event("input"));
    expect(onComment).toHaveBeenCalledWith(7, "make this bigger");
  });

  it("selecting an intent chip reports it and shows only that chip pressed", () => {
    const { host, pop, onChip } = harness();
    pop.open(ann({ index: 3 }));
    const chips = host.querySelectorAll<HTMLButtonElement>('.ap-annot-chips[data-kind="intent"] .ap-annot-chip');
    const change = [...chips].find((c) => c.dataset.val === "change")!;
    const bug = [...chips].find((c) => c.dataset.val === "bug")!;
    change.click();
    expect(onChip).toHaveBeenCalledWith(3, "intent", "change");
    expect(change.getAttribute("aria-pressed")).toBe("true");
    bug.click();
    expect(onChip).toHaveBeenCalledWith(3, "intent", "bug");
    expect(bug.getAttribute("aria-pressed")).toBe("true");
    expect(change.getAttribute("aria-pressed")).toBe("false"); // single-select within group
  });

  it("clicking the already-selected chip clears it", () => {
    const { host, pop, onChip } = harness();
    pop.open(ann({ index: 3, intent: "praise" }));
    const praise = host.querySelector<HTMLButtonElement>('.ap-annot-chips[data-kind="intent"] .ap-annot-chip[data-val="praise"]')!;
    expect(praise.getAttribute("aria-pressed")).toBe("true"); // reflects existing value
    praise.click();
    expect(onChip).toHaveBeenCalledWith(3, "intent", "");
    expect(praise.getAttribute("aria-pressed")).toBe("false");
  });

  it("Delete reports the annotation id", () => {
    const { host, pop, onDelete } = harness();
    pop.open(ann({ index: 9 }));
    host.querySelector<HTMLButtonElement>(".ap-annot-pop .ap-annot-del")!.click();
    expect(onDelete).toHaveBeenCalledWith(9);
  });

  it("Done closes the popover", () => {
    const { host, pop } = harness();
    pop.open(ann());
    host.querySelector<HTMLButtonElement>(".ap-annot-pop .ap-annot-done")!.click();
    expect(host.querySelector(".ap-annot-pop")).toBeNull();
    expect(pop.isOpen).toBe(false);
  });

  it("opening a second annotation replaces the first — only one popover exists", () => {
    const { host, pop } = harness();
    pop.open(ann({ index: 1 }));
    pop.open(ann({ index: 2 }));
    expect(host.querySelectorAll(".ap-annot-pop").length).toBe(1);
    expect(pop.openIndex).toBe(2);
  });
});
