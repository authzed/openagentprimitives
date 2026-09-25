import { describe, it, expect, vi } from "vitest";
import { Annotator } from "./annotator";

function docs() {
  const host = document.implementation.createHTMLDocument("host");
  const inner = document.implementation.createHTMLDocument("inner");
  inner.body.innerHTML = `<section class="card"><button id="cta">Go</button><button id="cta2">Stop</button></section>`;
  return { host, inner };
}

const cta = (inner: Document) => inner.querySelector("#cta")!;

describe("Annotator", () => {
  it("arming toggles state; a click adds a numbered pin into the inner doc", () => {
    const { host, inner } = docs();
    const a = new Annotator(host, inner, "artifact-9", () => {});
    a.arm();
    expect(a.armed).toBe(true);
    a.handleClick(cta(inner));
    expect(a.count).toBe(1);
    const pins = inner.querySelectorAll("[data-ap-pin]");
    expect(pins.length).toBe(1);
    expect(pins[0].textContent).toBe("1"); // stable id
  });

  it("a click opens the note editor in the host doc for that annotation", () => {
    const { host, inner } = docs();
    const a = new Annotator(host, inner, "x", () => {});
    a.arm();
    a.handleClick(cta(inner));
    const pop = host.querySelector(".ap-annot-pop");
    expect(pop).not.toBeNull();
    expect(pop!.getAttribute("data-ap-pop")).toBe("1");
  });

  it("clicking the SAME element again does not add a second annotation (reopens instead)", () => {
    const { host, inner } = docs();
    const a = new Annotator(host, inner, "x", () => {});
    a.arm();
    a.handleClick(cta(inner));
    a.handleClick(cta(inner));
    expect(a.count).toBe(1);
    expect(inner.querySelectorAll("[data-ap-pin]").length).toBe(1);
  });

  it("clicking a pin reopens its note editor instead of adding an annotation", () => {
    const { host, inner } = docs();
    const a = new Annotator(host, inner, "x", () => {});
    a.arm();
    a.handleClick(cta(inner)); // annotation 1
    host.querySelector(".ap-annot-pop .ap-annot-done")!.dispatchEvent(new Event("click")); // close it
    const pin = inner.querySelector("[data-ap-pin]")!;
    a.handleClick(pin as Element);
    expect(a.count).toBe(1); // no new annotation
    expect(host.querySelector<HTMLElement>(".ap-annot-pop")!.getAttribute("data-ap-pop")).toBe("1");
  });

  it("remove() drops the annotation and its pin", () => {
    const { host, inner } = docs();
    const a = new Annotator(host, inner, "x", () => {});
    a.arm();
    a.handleClick(cta(inner));
    expect(a.count).toBe(1);
    a.remove(1);
    expect(a.count).toBe(0);
    expect(inner.querySelectorAll("[data-ap-pin]").length).toBe(0);
  });

  it("disarm() removes the hover outline (no stuck highlight after leaving annotate mode)", () => {
    const { host, inner } = docs();
    const a = new Annotator(host, inner, "x", () => {});
    a.arm();
    a.hover(cta(inner));
    expect(inner.querySelector("[data-ap-outline]")).not.toBeNull();
    a.disarm();
    expect(inner.querySelector("[data-ap-outline]")).toBeNull();
  });

  it("Send is gated on a filled comment: no note → no emit; with a note → emits and clears", () => {
    const { host, inner } = docs();
    const onSend = vi.fn();
    const a = new Annotator(host, inner, "artifact-9", onSend);
    a.arm();
    a.handleClick(cta(inner));
    a.send();
    expect(onSend).not.toHaveBeenCalled(); // empty comment — nothing to send
    a.setComment(1, "reword this");
    a.send();
    expect(onSend).toHaveBeenCalledTimes(1);
    const bundle = onSend.mock.calls[0][0];
    expect(bundle.artifactId).toBe("artifact-9");
    expect(bundle.annotations[0].comment).toBe("reword this");
    expect(a.count).toBe(0); // cleared after hand-off
    expect(inner.querySelectorAll("[data-ap-pin]").length).toBe(0);
  });

  it("filledCount() counts commented annotations; subscribers are notified on change", () => {
    const { host, inner } = docs();
    const a = new Annotator(host, inner, "x", () => {});
    const seen = vi.fn();
    a.subscribe(seen);
    a.arm();
    a.handleClick(cta(inner));
    a.handleClick(inner.querySelector("#cta2")!);
    expect(a.count).toBe(2);
    expect(a.filledCount()).toBe(0);
    a.setComment(1, "note");
    expect(a.filledCount()).toBe(1);
    expect(seen).toHaveBeenCalled(); // arm + clicks + setComment all notified
  });

  it("reopenPin re-enters annotate mode and reopens the note (clicking a pin while disarmed)", () => {
    const { host, inner } = docs();
    const a = new Annotator(host, inner, "x", () => {});
    a.arm();
    a.handleClick(cta(inner)); // annotation 1
    a.disarm();
    expect(a.armed).toBe(false);
    a.reopenPin(1);
    expect(a.armed).toBe(true); // clicking our own pin re-enters annotate mode
    expect(host.querySelector<HTMLElement>(".ap-annot-pop")!.getAttribute("data-ap-pop")).toBe("1");
  });

  it("addComponent adds another pin to the OPEN annotation instead of a new one", () => {
    const { host, inner } = docs();
    const a = new Annotator(host, inner, "x", () => {});
    a.arm();
    a.handleClick(cta(inner));                     // annotation 1, its popover open
    a.addComponent(inner.querySelector("#cta2")!); // ⌥-click adds cta2 to annotation 1
    expect(a.count).toBe(1);                        // still one annotation
    const pins = inner.querySelectorAll("[data-ap-pin]");
    expect(pins.length).toBe(2);
    expect([...pins].every((p) => p.textContent === "1")).toBe(true); // both share the number
    expect(a.list()[0].extra?.length).toBe(1);
  });

  it("addComponent with no open annotation falls back to starting a new one", () => {
    const { host, inner } = docs();
    const a = new Annotator(host, inner, "x", () => {});
    a.arm();
    a.addComponent(cta(inner)); // nothing open → behaves like a normal click
    expect(a.count).toBe(1);
    expect(inner.querySelectorAll("[data-ap-pin]").length).toBe(1);
  });

  it("remove() clears ALL pins of a multi-component annotation", () => {
    const { host, inner } = docs();
    const a = new Annotator(host, inner, "x", () => {});
    a.arm();
    a.handleClick(cta(inner));
    a.addComponent(inner.querySelector("#cta2")!);
    expect(inner.querySelectorAll("[data-ap-pin]").length).toBe(2);
    a.remove(1);
    expect(a.count).toBe(0);
    expect(inner.querySelectorAll("[data-ap-pin]").length).toBe(0);
  });

  it("addSelection captures the selected text as a selection annotation + pin + popover", () => {
    const { host, inner } = docs();
    const a = new Annotator(host, inner, "x", () => {});
    a.arm();
    const el = inner.querySelector("#cta")!;
    (inner as unknown as { getSelection: () => unknown }).getSelection = () => ({
      isCollapsed: false, rangeCount: 1,
      toString: () => "Go now",
      getRangeAt: () => ({ commonAncestorContainer: el }),
    });
    a.addSelection();
    expect(a.count).toBe(1);
    const ann = a.list()[0];
    expect(ann.target).toBe("selection");
    expect(ann.selectedText).toBe("Go now");
    expect(inner.querySelectorAll("[data-ap-pin]").length).toBe(1);
    expect(host.querySelector(".ap-annot-pop")).not.toBeNull();
  });
});
