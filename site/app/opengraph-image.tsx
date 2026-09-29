import { ImageResponse } from "next/og";
import { OG_IMAGE } from "@/lib/site";

// The share card for every page that does not ship its own. Rendered once at
// build time. The logomark geometry is the same as components/OapMark.tsx.
export const alt = OG_IMAGE.alt;
export const size = { width: 1200, height: 630 };
export const contentType = "image/png";

// The renderer's default font sets an ordinary space far wider than the
// non-breaking one, so the headline's spaces are swapped before rendering.
const nb = (text: string) => text.replaceAll(" ", "\u00a0");

export default function OpengraphImage() {
  return new ImageResponse(
    <div
      style={{
        width: "100%",
        height: "100%",
        display: "flex",
        flexDirection: "column",
        justifyContent: "space-between",
        background: "#0c050f",
        color: "#f8f7f8",
        padding: "72px 80px",
      }}
    >
      <svg width="180" height="71" viewBox="0 0 1000 393" fill="#f1f0f2">
        <path d="M160.219 0C248.705 2.34906e-05 320.437 71.7324 320.438 160.219C320.437 248.705 248.705 320.437 160.219 320.438C71.7324 320.438 2.35732e-05 248.705 0 160.219C-1.21838e-05 71.7324 71.7324 1.1051e-05 160.219 0ZM170.219 90.417C164.031 86.8444 156.407 86.8445 150.219 90.417L104.769 116.658C98.5808 120.231 94.7686 126.833 94.7686 133.979V186.459C94.7686 193.604 98.5808 200.207 104.769 203.779L150.219 230.021C156.407 233.593 164.031 233.593 170.219 230.021L215.669 203.779C221.857 200.207 225.669 193.604 225.669 186.459V133.979C225.669 126.833 221.857 120.231 215.669 116.658L170.219 90.417Z" />
        <path d="M478.478 19.8457C484.286 9.71696 498.895 9.71695 504.703 19.8457L661.864 293.951C667.642 304.028 660.367 316.585 648.752 316.585H334.43C322.814 316.585 315.54 304.028 321.317 293.951L478.478 19.8457ZM498.147 145.475C495.244 140.41 487.939 140.41 485.035 145.475L436.547 230.043C433.658 235.081 437.295 241.359 443.102 241.359H540.079C545.887 241.359 549.524 235.081 546.636 230.043L498.147 145.475Z" />
        <path d="M977.327 12.2494C989.849 12.2494 1000 22.3997 1000 34.9213V278.912C1000 291.434 989.849 301.585 977.327 301.585H826.933C814.412 301.586 804.262 311.736 804.262 324.257V369.663C804.262 382.185 794.11 392.336 781.589 392.336H721.129C708.607 392.336 698.457 382.185 698.457 369.663V34.9213C698.457 22.3998 708.607 12.2496 721.129 12.2494H977.327ZM762.693 143.302C762.693 151.65 769.46 158.417 777.808 158.417H860.646C868.993 158.417 875.761 165.185 875.761 173.532V208.245C875.761 216.593 882.528 223.36 890.876 223.36H920.646C928.993 223.36 935.761 216.593 935.761 208.245V113.532C935.761 105.185 928.993 98.4174 920.646 98.4174H777.808C769.46 98.4174 762.693 105.185 762.693 113.532V143.302Z" />
      </svg>
      <div style={{ display: "flex", flexDirection: "column" }}>
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
          <div style={{ display: "flex", color: "#72b1ad" }}>
            {nb("enterprise AI agents.")}
          </div>
        </div>
        <div style={{ fontSize: 30, color: "#9a94a0", marginTop: 28 }}>
          Open Agent Primitives · openap.org
        </div>
      </div>
    </div>,
    size,
  );
}
