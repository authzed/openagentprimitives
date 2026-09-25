// In-page capture overlay: an animated pointer, a highlight box, and a
// lower-third caption bar. These are drawn as fixed-position DOM appended to
// <body> and mutated imperatively (not through React) so the capture engine can
// move the cursor at 60Hz without triggering re-renders. Everything here is a
// capture aid — it is not part of the scenario data model.

export interface Rect {
  x: number;
  y: number;
  width: number;
  height: number;
}

export interface OverlayControl {
  /** Move the pointer to viewport coords; `down` shows the click-press ring. */
  pointer: (x: number, y: number, down?: boolean) => void;
  hidePointer: () => void;
  /** Draw a highlight box around a rect, or clear it with null. */
  highlight: (rect: Rect | null) => void;
  /** Set the narration caption text, or hide it with null. */
  caption: (text: string | null) => void;
}

const CURSOR_SVG = `
<svg width="26" height="26" viewBox="0 0 26 26" fill="none" xmlns="http://www.w3.org/2000/svg">
  <path d="M5 3l14 7-6 1.5L10 20 5 3z" fill="#111" stroke="#fff" stroke-width="1.5" stroke-linejoin="round"/>
</svg>`;

export function installOverlay(): OverlayControl {
  const root = document.createElement("div");
  root.className = "sk-overlay";
  root.innerHTML = `
    <div class="sk-ov-highlight" hidden></div>
    <div class="sk-ov-cursor" hidden>${CURSOR_SVG}<span class="sk-ov-ring"></span></div>
    <div class="sk-ov-caption" hidden><span class="sk-ov-caption-text"></span></div>
  `;
  document.body.appendChild(root);

  const cursor = root.querySelector(".sk-ov-cursor") as HTMLElement;
  const highlightEl = root.querySelector(".sk-ov-highlight") as HTMLElement;
  const captionEl = root.querySelector(".sk-ov-caption") as HTMLElement;
  const captionText = root.querySelector(".sk-ov-caption-text") as HTMLElement;

  return {
    pointer(x, y, down = false) {
      cursor.hidden = false;
      cursor.style.transform = `translate(${x}px, ${y}px)`;
      cursor.classList.toggle("is-down", down);
    },
    hidePointer() {
      cursor.hidden = true;
    },
    highlight(rect) {
      if (!rect) {
        highlightEl.hidden = true;
        return;
      }
      highlightEl.hidden = false;
      highlightEl.style.transform = `translate(${rect.x}px, ${rect.y}px)`;
      highlightEl.style.width = `${rect.width}px`;
      highlightEl.style.height = `${rect.height}px`;
    },
    caption(text) {
      if (!text) {
        captionEl.hidden = true;
        return;
      }
      captionEl.hidden = false;
      captionText.textContent = text;
    },
  };
}

// consolesim binds the OverlayControl as window.__showcase so the shared capture
// engine (clip.mjs / still.mjs) can set captions and move the pointer — the same
// handle slacksim exposes (there via its store control). Only caption/pointer are
// used by the engine. The global itself is declared in demos/showcase-globals.d.ts.
