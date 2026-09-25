// Entry for the sandbox-origin annotation renderer. It runs inside the D1 host
// page and annotates the inner artifact frame same-origin. Config arrives on
// window.__AP_ANNOT (set by a nonce'd inline script in host.go).
//
// Vanilla TS (no JSX) — this bundle runs in the tightly-locked-down host page,
// not through @ap/runtime's React mount helper. All interaction wiring lives in
// bootAnnotator (boot.ts), shared with the dev harness (web/dev/annotator).
import { bootAnnotator } from "./boot";

declare global { interface Window { __AP_ANNOT?: { shell: string } } }

// host.go only emits window.__AP_ANNOT (and this bundle's <script src>) when
// the session's class grants the annotation_batch interaction, so cfg being
// absent already means "not granted" in the normal case. This guard is
// defense-in-depth for that contract, not the primary gate.
const cfg = window.__AP_ANNOT;
const innerFrame = document.getElementById("ap-artifact") as HTMLIFrameElement | null;
if (cfg?.shell && innerFrame) {
  // artifactId is stamped by the SHELL (which holds it + the server re-checks
  // it); the sandbox never needs it, so pass "" here.
  bootAnnotator({ hostDoc: document, innerFrame, shellOrigin: cfg.shell, artifactId: "" });
}
