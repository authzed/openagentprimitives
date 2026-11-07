"use client";
import { useState, type ReactNode } from "react";
import type { MediaAsset } from "@/lib/manifest";

// The interactive pieces of a story page. Everything else is server-rendered;
// these hydrate in place.

// "PT1M42S" → "1:42".
function clipLength(iso?: string): string | null {
  const m = iso?.match(/^PT(?:(\d+)M)?(?:(\d+)S)?$/);
  if (!m) return null;
  return `${Number(m[1] ?? 0)}:${String(Number(m[2] ?? 0)).padStart(2, "0")}`;
}

// The hero clip: a still with a "Watch the demo" pill until clicked, then the
// narrated video itself, started unmuted (narration is the point).
export function DemoClip({
  asset,
  still,
  alt,
}: {
  asset: MediaAsset;
  still: string;
  alt: string;
}) {
  const [playing, setPlaying] = useState(false);
  const length = clipLength(asset.duration);
  return (
    <div className="clip-frame">
      {playing ? (
        <video
          controls
          autoPlay
          playsInline
          poster={asset.poster}
          style={{ display: "block", width: "100%", height: "auto" }}
        >
          {asset.webm && <source src={asset.webm} type="video/webm" />}
          {asset.mp4 && <source src={asset.mp4} type="video/mp4" />}
        </video>
      ) : (
        <button
          type="button"
          className="clip-start"
          onClick={() => setPlaying(true)}
          aria-label="Watch the demo"
        >
          <img src={still} alt={alt} />
          <span className="clip-play" aria-hidden="true">
            <span>
              <i>
                <svg width="10" height="11" viewBox="0 0 22 24">
                  <path d="M2 2l18 10L2 22z" fill="currentColor" />
                </svg>
              </i>
              Watch the demo {length && <em>{length}</em>}
            </span>
          </span>
        </button>
      )}
    </div>
  );
}

// Revision 1 ↔ revision 2: the stage cross-fades between the two shots and
// the side panel swaps between the notes and the change note.
export function BeforeAfter({
  rev1,
  rev2,
  panel1,
  panel2,
}: {
  rev1: { src: string; alt: string };
  rev2: { src: string; alt: string };
  panel1: ReactNode;
  panel2: ReactNode;
}) {
  const [rev, setRev] = useState<"1" | "2">("1");
  const tab = (n: "1" | "2", title: string, sub: string) => (
    <button
      type="button"
      role="tab"
      id={`ba-tab-${n}`}
      aria-controls={`ba-panel-${n}`}
      aria-selected={rev === n}
      onClick={() => setRev(n)}
    >
      <b>{title}</b>
      {sub}
    </button>
  );
  return (
    <div className="ba" data-rev={rev}>
      {/* On a phone the shot keeps a readable width and pans sideways. */}
      <div className="ba-stage" tabIndex={0} aria-label="Revision screenshot">
        <div className="ba-shot">
          <img className="rev1" src={rev1.src} alt={rev1.alt} />
          <img className="rev2" src={rev2.src} alt={rev2.alt} />
        </div>
      </div>
      <div className="ba-side">
        <div className="ba-tabs" role="tablist">
          {tab("1", "Revision 1", "2 notes to send")}
          {tab("2", "Revision 2", "both answered")}
        </div>
        <div
          className="ba-panel p1"
          role="tabpanel"
          id="ba-panel-1"
          aria-labelledby="ba-tab-1"
        >
          {panel1}
        </div>
        <div
          className="ba-panel p2"
          role="tabpanel"
          id="ba-panel-2"
          aria-labelledby="ba-tab-2"
        >
          {panel2}
        </div>
      </div>
    </div>
  );
}

export function CopyButton({ text }: { text: string }) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
    } catch {
      const ta = Object.assign(document.createElement("textarea"), {
        value: text,
      });
      ta.style.cssText = "position:fixed;opacity:0";
      document.body.append(ta);
      ta.select();
      document.execCommand("copy");
      ta.remove();
    }
    setCopied(true);
    setTimeout(() => setCopied(false), 1200);
  };
  return (
    <button className="copy" type="button" onClick={copy}>
      {copied ? "Copied" : "Copy"}
    </button>
  );
}
