import type { Annotator } from "./annotator";
import { annotationLabel } from "./popover";

// buildToolbar creates the annotation chrome in the HOST document. It starts
// COLLAPSED to a small trigger (the fab); clicking it slides the panel open
// leftward, and it stays open until the trigger is clicked again. The panel holds
// the review list (edit/remove each), the Annotate toggle, a count, and Send. It
// re-renders on annotator changes via subscribe(); the trigger carries a pending
// count so the collapsed state still signals there's work.
//
// Styled with CSS CLASSES only (never inline style="" attributes): the host
// page's CSP is style-src 'nonce-X' with no 'unsafe-inline', so an inline style
// would be stripped. The rules ship as a nonce'd <style> from host.go
// (annotatorCSS); keep the class names in lockstep.
export function buildToolbar(hostDoc: Document, a: Annotator): HTMLElement {
  const bar = hostDoc.createElement("div");
  bar.className = "ap-annot-toolbar";

  const panel = hostDoc.createElement("div");
  panel.className = "ap-annot-panel";

  const list = hostDoc.createElement("ul");
  list.className = "ap-annot-list";

  const row = hostDoc.createElement("div");
  row.className = "ap-annot-row";

  const toggle = hostDoc.createElement("button");
  toggle.className = "ap-annot-btn ap-annot-toggle";
  toggle.setAttribute("aria-pressed", "false");

  const count = hostDoc.createElement("span");
  count.className = "ap-annot-count";

  const send = hostDoc.createElement("button");
  send.className = "ap-annot-btn ap-annot-send";
  send.textContent = "Send";

  row.append(toggle, count, send);
  panel.append(list, row);

  // The trigger. Panel first, fab second → the panel opens leftward from the
  // fixed right edge. The fab stays put; it toggles the panel open/closed.
  const fab = hostDoc.createElement("button");
  fab.className = "ap-annot-btn ap-annot-fab";
  fab.setAttribute("aria-label", "Annotations");
  const fabIcon = hostDoc.createElement("span");
  fabIcon.className = "ap-annot-fab-icon";
  const fabCount = hostDoc.createElement("span");
  fabCount.className = "ap-annot-fab-count";
  fab.append(fabIcon, fabCount);

  bar.append(panel, fab);

  let open = false; // collapsed until the trigger is clicked; then sticky-open
  fab.onclick = () => { open = !open; render(); };
  toggle.onclick = () => { a.armed ? a.disarm() : a.arm(); };
  send.onclick = () => { a.send(); };

  const render = () => {
    // Open/closed chrome state (drives the leftward slide + the fab icon).
    bar.classList.toggle("ap-annot-open", open);
    fab.setAttribute("aria-expanded", open ? "true" : "false");
    fabIcon.textContent = open ? "›" : "‹";

    // Toggle: label + colour (via aria-pressed) reflect annotate mode.
    toggle.textContent = a.armed ? "● Annotating" : "Annotate";
    toggle.setAttribute("aria-pressed", a.armed ? "true" : "false");

    // Counts: filled/total in the row; total on the collapsed trigger badge.
    const total = a.count, filled = a.filledCount();
    count.textContent = total ? `${filled}/${total}` : "";
    fabCount.textContent = total ? String(total) : "";
    fabCount.hidden = total === 0;

    // Send is only meaningful once at least one annotation carries a note.
    send.disabled = filled === 0;

    // Rebuild the review list.
    list.textContent = "";
    for (const ann of a.list()) {
      const item = hostDoc.createElement("li");
      item.className = "ap-annot-item";
      item.setAttribute("data-idx", String(ann.index));

      const n = hostDoc.createElement("span");
      n.className = "ap-annot-item-n";
      n.textContent = String(ann.index);

      const label = hostDoc.createElement("span");
      label.className = "ap-annot-item-label";
      label.textContent = annotationLabel(ann, 28);

      const note = hostDoc.createElement("span");
      note.className = ann.comment.trim() ? "ap-annot-item-note" : "ap-annot-item-note ap-annot-empty";
      note.textContent = ann.comment.trim() || "no note yet";

      const edit = hostDoc.createElement("button");
      edit.className = "ap-annot-edit";
      edit.textContent = "Edit";
      edit.onclick = () => a.openPopoverFor(ann.index);

      const remove = hostDoc.createElement("button");
      remove.className = "ap-annot-remove";
      remove.setAttribute("aria-label", `Remove annotation ${ann.index}`);
      remove.textContent = "✕";
      remove.onclick = () => a.remove(ann.index);

      item.append(n, label, note, edit, remove);
      list.appendChild(item);
    }
  };

  a.subscribe(render);
  render();

  (hostDoc.body || hostDoc.documentElement).appendChild(bar);
  return bar;
}
