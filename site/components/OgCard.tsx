import type { ReactNode } from "react";
import { OAP_MARK_PATHS } from "./OapMark";

// The share-card layout behind app/opengraph-image.tsx (the site card) and
// app/docs/[slug]/opengraph-image.tsx (one per guide). Rendered by next/og at
// build time, which supports only flexbox and inline styles.
export const OG_SIZE = { width: 1200, height: 630 };

export const OG_INK = {
  bg: "#0c050f",
  text: "#f8f7f8",
  mark: "#f1f0f2",
  accent: "#72b1ad",
  muted: "#9a94a0",
};

// The renderer's default font sets an ordinary space far wider than the
// non-breaking one. Where a line must still wrap, a zero-width space after
// each non-breaking one keeps a break opportunity.
export const nb = (text: string) => text.replaceAll(" ", " ");
export const nbWrap = (text: string) => text.replaceAll(" ", " ​");

export function OgCard({
  children,
  footer,
}: {
  children: ReactNode;
  footer: string;
}) {
  return (
    <div
      style={{
        width: "100%",
        height: "100%",
        display: "flex",
        flexDirection: "column",
        justifyContent: "space-between",
        background: OG_INK.bg,
        color: OG_INK.text,
        padding: "72px 80px",
      }}
    >
      <svg width="180" height="71" viewBox="0 0 1000 393" fill={OG_INK.mark}>
        {OAP_MARK_PATHS.map((d) => (
          <path key={d} d={d} />
        ))}
      </svg>
      <div style={{ display: "flex", flexDirection: "column" }}>
        {children}
        <div style={{ fontSize: 30, color: OG_INK.muted, marginTop: 28 }}>
          {nb(footer)}
        </div>
      </div>
    </div>
  );
}
