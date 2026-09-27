// Dev-only. Plays the trusted shell around the REAL annotator: builds a fake
// artifact in the inner frame, boots the annotator over it, and catches the
// annotator's real postMessage envelope instead of POSTing to /interact.
import { bootAnnotator } from "../../../pkg/web/webui/artifactview/ui/host/boot";

const FIXTURES = ["rich", "form", "long", "minimal"] as const;
type Fixture = (typeof FIXTURES)[number];

const frame = document.getElementById("ap-artifact") as HTMLIFrameElement;
const panel = document.getElementById("bundle-panel") as HTMLElement;
const logEl = document.getElementById("send-log") as HTMLElement;
const picker = document.getElementById("fixture") as HTMLSelectElement;
const swapBtn = document.getElementById("swap") as HTMLButtonElement;

let sends = 0;

function load(fixture: Fixture): void {
  // Setting src re-navigates the SAME iframe and fires 'load' — exactly what the
  // live-view bridge (hostBridgeJS) does on a revision swap, so this also
  // exercises bootAnnotator's re-boot/teardown path.
  frame.src = `/fixtures/${fixture}.html`;
}

// Minimal round-trip echo: briefly flash the annotated elements in the frame so
// the loop is visible. Best-effort — a captured selector may not resolve.
function echo(bundle: { annotations: Array<{ elementPath: string }> }): void {
  const doc = frame.contentDocument;
  const win = frame.contentWindow as (Window & typeof globalThis) | null;
  if (!doc || !win) return;
  for (const a of bundle.annotations) {
    try {
      const el = doc.querySelector(a.elementPath);
      if (el instanceof win.HTMLElement) {
        el.style.outline = "3px solid #16a34a";
        el.style.transition = "outline .2s";
        win.setTimeout(() => {
          el.style.outline = "";
        }, 900);
      }
    } catch {
      /* invalid/unmatched selector — ignore */
    }
  }
}

// The shell receiver. window.parent === window here (single page), so the
// annotator's postMessage(..., location.origin) lands right back on us.
window.addEventListener("message", (e: MessageEvent) => {
  if (e.origin !== location.origin) return;
  const d = e.data;
  if (!d || d.ap !== "annot" || d.cmd !== "send") return;
  sends++;
  panel.textContent = JSON.stringify(d.bundle, null, 2);
  const li = document.createElement("li");
  li.textContent = `#${sends} · ${d.bundle.annotations.length} annotation(s) · ${new Date().toLocaleTimeString()}`;
  logEl.prepend(li);
  echo(d.bundle);
});

// DOM contract the annotator reads, then boot the real annotator.
(window as unknown as { __AP_ANNOT?: { shell: string } }).__AP_ANNOT = {
  shell: location.origin,
};
bootAnnotator({
  hostDoc: document,
  innerFrame: frame,
  shellOrigin: location.origin,
  artifactId: "dev",
});

picker.addEventListener("change", () => load(picker.value as Fixture));
swapBtn.addEventListener("click", () => load(picker.value as Fixture));

load("rich");
