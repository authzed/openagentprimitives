import { Annotator } from "./annotator";
import { buildToolbar } from "./toolbar";
import type { Bundle } from "./bundle";

export interface BootOptions {
  hostDoc: Document;
  innerFrame: HTMLIFrameElement;
  shellOrigin: string;
  artifactId: string;
}

// bootAnnotator wires the annotator over one inner artifact frame. Both the
// production host entry (index.tsx) and the dev harness call this — it is the
// single testable seam for the interaction glue.
//
// The D1 host bridge (hostBridgeJS in host.go) resets innerFrame's `src` on every
// live-view revision swap, which fires a fresh "load" on the SAME iframe element
// for a brand-new contentDocument. boot() must re-run on every one of those loads
// (not just the first) so the annotator always targets the current document —
// otherwise it either stacks a second toolbar (if we listened) or keeps annotating
// a stale, detached document (if we only ran once at eval time).
export function bootAnnotator(opts: BootOptions): void {
  const { hostDoc, innerFrame, shellOrigin, artifactId } = opts;
  let currentToolbar: HTMLElement | null = null;
  let lastDoc: Document | null = null;

  const boot = () => {
    const innerDoc = innerFrame.contentDocument; // same-origin (D1)
    // Guard against double-booting the SAME document: the immediate
    // already-complete check below and the "load" listener can otherwise both
    // fire for one document instance.
    if (!innerDoc || innerDoc === lastDoc) return;
    lastDoc = innerDoc;

    // The toolbar lives in the HOST document, which persists across swaps (only
    // the INNER document is replaced) — tear down whatever this module previously
    // added before building a fresh one, or toolbars stack on every swap. Any pins
    // injected into the previous innerDoc need no explicit cleanup: that document
    // (and everything in it) is gone the moment the frame navigates.
    if (currentToolbar) {
      currentToolbar.parentNode?.removeChild(currentToolbar);
      currentToolbar = null;
    }

    const annotator = new Annotator(hostDoc, innerDoc, artifactId, (b: Bundle) => {
      try {
        hostDoc.defaultView?.parent.postMessage({ ap: "annot", cmd: "send", bundle: b }, shellOrigin);
      } catch { /* no-op */ }
    });
    currentToolbar = buildToolbar(hostDoc, annotator);
    wireInner(innerDoc, annotator);
  };

  // Always listen — every revision swap fires "load" again on this same element —
  // AND run once immediately if the initial content is already complete at
  // script-eval time, so the very first mount isn't missed.
  innerFrame.addEventListener("load", boot);
  if (innerFrame.contentDocument?.readyState === "complete") boot();
}

function wireInner(innerDoc: Document, a: Annotator): void {
  // Hover highlight — the annotator owns the outline element (so disarm() can
  // tear it down); this just forwards the element under the cursor.
  innerDoc.addEventListener("pointermove", (e) => {
    const el = e.target as Element | null;
    if (el) a.hover(el);
  });
  // Text selection: annotate on mouseup while the selection still exists — the
  // click that follows would collapse it. A plain click leaves nothing selected,
  // so this only fires for a real drag-select.
  innerDoc.addEventListener("mouseup", () => {
    if (a.armed && innerDoc.getSelection()?.isCollapsed === false) a.addSelection();
  });
  innerDoc.addEventListener("click", (e) => {
    const t = e.target as Element | null;
    // A click on one of our pins is unambiguous — reopen it (and re-enter annotate
    // mode) even when not armed. It never collides with normal artifact reading.
    const pinEl = t?.closest?.("[data-ap-pin]");
    if (pinEl) {
      e.preventDefault(); e.stopPropagation();
      a.reopenPin(Number(pinEl.getAttribute("data-ap-pin")));
      return;
    }
    if (!a.armed || !t) return; // otherwise only capture new annotations while armed
    e.preventDefault(); e.stopPropagation();
    // ⌥/Alt-click adds the element to the open annotation as another component;
    // a plain click starts/reopens one. (Selections were handled on mouseup.)
    if ((e as MouseEvent).altKey) { a.addComponent(t); return; }
    if (innerDoc.getSelection()?.isCollapsed !== false) a.handleClick(t);
  }, true);
}
