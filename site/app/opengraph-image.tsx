import { ImageResponse } from "next/og";
import { nb, OG_INK, OG_SIZE, OgCard } from "@/components/OgCard";
import { OG_IMAGE } from "@/lib/site";

// The share card for every page that does not ship its own. Rendered once at
// build time.
export const alt = OG_IMAGE.alt;
export const size = OG_SIZE;
export const contentType = "image/png";

export default function OpengraphImage() {
  return new ImageResponse(
    <OgCard footer="Open Agent Primitives · openap.org">
      <div
        style={{
          fontSize: 76,
          lineHeight: 1.05,
          letterSpacing: "-0.03em",
          display: "flex",
          flexDirection: "column",
        }}
      >
        <div style={{ display: "flex" }}>{nb("A secure way to run")}</div>
        <div style={{ display: "flex", color: OG_INK.accent }}>
          {nb("enterprise AI agents.")}
        </div>
      </div>
    </OgCard>,
    size,
  );
}
