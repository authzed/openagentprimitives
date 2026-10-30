import { useLayoutEffect, useRef, useState } from "react";
import { Download, Menu, Send } from "lucide-react";
import type { ArtifactView } from "../store/types";

// A faithful MOCK of OAP's artifact live viewer
// (pkg/web/webui/artifactview/ui/ArtifactView.tsx) and its annotation layer
// (ui/host/annotator.ts), opened over the client the way "View live" opens a
// browser tab. Authored for the demos — the viewer and annotations are real
// features, but this chrome is illustrative, not a live capture. It draws the
// same pieces the real one does: the title bar with revision status, the
// revision history (newest first, `latest` badged), the rendered artifact on
// a white canvas, numbered pins on annotated elements, and the batch Send.
export function ArtifactViewer({ artifact }: { artifact: ArtifactView }) {
  const canvasRef = useRef<HTMLDivElement>(null);
  const [popover, setPopover] = useState<{ top: number; left: number } | null>(
    null,
  );
  const newest = artifact.annotations[artifact.annotations.length - 1];

  // Pin each annotated element (a numbered badge, via a data attribute the CSS
  // reads) and place the newest note's popover beside its element. Runs after
  // every render because the artifact body is injected HTML, not React nodes.
  useLayoutEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas) return;
    canvas
      .querySelectorAll("[data-av-pin]")
      .forEach((el) => el.removeAttribute("data-av-pin"));
    for (const a of artifact.annotations) {
      canvas
        .querySelector(`#${a.targetId}`)
        ?.setAttribute("data-av-pin", String(a.index));
    }
    const target =
      newest && !artifact.sentTo
        ? canvas.querySelector<HTMLElement>(`#${newest.targetId}`)
        : null;
    if (!target) {
      setPopover(null);
      return;
    }
    const c = canvas.getBoundingClientRect();
    const r = target.getBoundingClientRect();
    setPopover({
      top: r.bottom - c.top + canvas.scrollTop + 10,
      left: Math.min(r.left - c.left, c.width - 300),
    });
  }, [artifact, newest]);

  const pending = artifact.annotations.length;

  return (
    <div
      className="av-app"
      role="dialog"
      aria-label={`${artifact.title} — live view`}
    >
      <header className="av-bar">
        <span className="av-icon">
          <Menu size={16} />
        </span>
        <div className="av-title">
          <b>{artifact.title}</b>
          <span>{artifact.description}</span>
        </div>
        <span className="av-vdiv" />
        <span className="av-status">Ready · revision {artifact.revision}</span>
        <span className="av-spacer" />
        <span className="av-icon" title="Download">
          <Download size={16} />
        </span>
        <span className="av-live">
          <span className="av-dot" /> Live
        </span>
      </header>

      <div className="av-body">
        <aside className="av-side">
          <h4>Revisions</h4>
          {artifact.revisions.map((rev) => (
            <div
              key={rev.n}
              className={`av-rev ${rev.n === artifact.revision ? "is-sel" : ""}`.trim()}
            >
              <div className="av-rev-h">
                Revision {rev.n}
                {rev.n === artifact.revision && (
                  <span className="av-tag">latest</span>
                )}
              </div>
              <div className="av-rev-m">{rev.note}</div>
            </div>
          ))}
        </aside>

        <main className="av-canvas" ref={canvasRef}>
          <div
            className="av-doc"
            dangerouslySetInnerHTML={{ __html: artifact.html }}
          />
          {popover && newest && (
            <div className="av-pop" style={popover}>
              <div className="av-pop-who">
                <span className="av-pop-n">{newest.index}</span>
                <b>{newest.author}</b>
                {newest.intent && (
                  <span className="av-chip">{newest.intent}</span>
                )}
              </div>
              {newest.comment}
            </div>
          )}
        </main>

        <aside className="av-panel">
          <h4>
            Annotations
            <span className="av-count">
              {artifact.sentTo
                ? "sent"
                : pending === 0
                  ? "none yet"
                  : `${pending} to send`}
            </span>
          </h4>
          <div className="av-notes">
            {pending === 0 && (
              <p className="av-empty">
                Click any element of the page to pin a note to it.
              </p>
            )}
            {artifact.annotations.map((a) => (
              <div key={a.index} className="av-ncard">
                <div className="av-ref">
                  <span className="av-pop-n">{a.index}</span> on “{a.label}”
                  {a.intent && <span className="av-chip">{a.intent}</span>}
                </div>
                <div className="av-txt">{a.comment}</div>
              </div>
            ))}
          </div>
          <div className="av-send">
            {artifact.sentTo ? (
              <div className="av-sent">
                Sent to {artifact.sentTo} as one batch · revising…
              </div>
            ) : (
              <button
                className="sk-btn sk-btn--primary av-send-btn"
                data-action-id="send_annotations"
                disabled={pending === 0}
              >
                <Send size={14} /> Send {pending}{" "}
                {pending === 1 ? "note" : "notes"}
              </button>
            )}
          </div>
        </aside>
      </div>
    </div>
  );
}
