import { describe, it, expect } from "vitest";
import { bootAnnotator } from "./boot";

// A minimal stand-in for an <iframe> whose contentDocument we can swap and whose
// "load" we can fire on demand — jsdom cannot renavigate a real iframe in-place.
function stubFrame(doc: Document) {
  const listeners: Record<string, EventListener[]> = {};
  const frame: any = {
    contentDocument: doc,
    addEventListener(type: string, fn: EventListener) {
      (listeners[type] ??= []).push(fn);
    },
    fire(type: string) {
      for (const fn of listeners[type] ?? []) fn(new Event(type));
    },
  };
  return frame as HTMLIFrameElement & { fire(type: string): void };
}

// jsdom's document.implementation.createHTMLDocument() does NOT report
// readyState "complete", so force it where a test needs the "content already
// loaded at boot time" branch.
function innerDoc(name: string, complete = false): Document {
  const d = document.implementation.createHTMLDocument(name);
  if (complete) Object.defineProperty(d, "readyState", { value: "complete", configurable: true });
  return d;
}

const opts = (host: Document, frame: HTMLIFrameElement) => ({
  hostDoc: host,
  innerFrame: frame,
  shellOrigin: "http://localhost",
  artifactId: "dev",
});

describe("bootAnnotator", () => {
  it("mounts one toolbar immediately when the inner content is already complete", () => {
    const host = document.implementation.createHTMLDocument("host");
    const frame = stubFrame(innerDoc("inner", true));
    bootAnnotator(opts(host, frame)); // no load event — the readyState branch fires
    expect(host.querySelectorAll(".ap-annot-toolbar").length).toBe(1);
  });

  it("mounts one toolbar on the frame's load event", () => {
    const host = document.implementation.createHTMLDocument("host");
    const frame = stubFrame(innerDoc("inner"));
    bootAnnotator(opts(host, frame));
    expect(host.querySelectorAll(".ap-annot-toolbar").length).toBe(0); // not complete yet
    frame.fire("load");
    expect(host.querySelectorAll(".ap-annot-toolbar").length).toBe(1);
  });

  it("re-firing load for the SAME document does not stack a second toolbar", () => {
    const host = document.implementation.createHTMLDocument("host");
    const frame = stubFrame(innerDoc("inner"));
    bootAnnotator(opts(host, frame));
    frame.fire("load"); // boot #1 → mounts one
    frame.fire("load"); // same contentDocument → deduped, no second toolbar
    expect(host.querySelectorAll(".ap-annot-toolbar").length).toBe(1);
  });

  it("a revision swap (new document) tears down the old toolbar and mounts a fresh one", () => {
    const host = document.implementation.createHTMLDocument("host");
    const frame = stubFrame(innerDoc("A"));
    bootAnnotator(opts(host, frame));
    frame.fire("load"); // boot A
    const first = host.querySelector(".ap-annot-toolbar");
    expect(first).not.toBeNull();

    (frame as any).contentDocument = innerDoc("B");
    frame.fire("load"); // boot B → old toolbar removed, new one built
    const bars = host.querySelectorAll(".ap-annot-toolbar");
    expect(bars.length).toBe(1); // still exactly one — not stacked
    expect(bars[0]).not.toBe(first); // and it's a freshly built element
  });
});
