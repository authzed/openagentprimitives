import * as React from "react";
import { cn } from "../../lib/utils";

// The OAP brand marks, inlined from the 2026-09-23 finals in
// docs/assets/brand/ (update both together). Both light and dark exports share
// one geometry and differ only in fill, so a single path painted with
// currentColor replaces four files: the mark takes whatever colour the
// surrounding text has and never needs a theme-specific asset. Decorative by
// default (aria-hidden) — pass `title` when the mark is the only thing naming
// the product on a surface.

// OapMark is the icon: the triangle with the cut-out. Use it wherever a square
// slot wants the brand (header tile, favicon, sign-in card).
export function OapMark({
  className,
  title,
  ...props
}: React.SVGProps<SVGSVGElement> & { title?: string }) {
  return (
    <svg
      viewBox="0 0 345 305"
      fill="currentColor"
      aria-hidden={title ? undefined : true}
      role={title ? "img" : undefined}
      className={cn("shrink-0", className)}
      {...props}
    >
      {title ? <title>{title}</title> : null}
      <path d="M159.186 7.59677C164.993 -2.53226 179.604 -2.53226 185.411 7.59677L342.572 281.702C348.349 291.779 341.075 304.335 329.459 304.335H15.138C3.52262 304.335 -3.75273 291.779 2.02468 281.702L159.186 7.59677ZM178.855 133.226C175.951 128.161 168.646 128.161 165.742 133.226L117.254 217.794C114.366 222.832 118.003 229.11 123.811 229.11H220.786C226.594 229.11 230.232 222.832 227.343 217.794L178.855 133.226Z" />
    </svg>
  );
}

// OapLogomark is the O·A·P letterforms as primitives (circle, triangle, bowl).
// Wide (1000×393); use it where there is horizontal room and no wordmark text.
export function OapLogomark({
  className,
  title,
  ...props
}: React.SVGProps<SVGSVGElement> & { title?: string }) {
  return (
    <svg
      viewBox="0 0 1000 393"
      fill="currentColor"
      aria-hidden={title ? undefined : true}
      role={title ? "img" : undefined}
      className={cn("shrink-0", className)}
      {...props}
    >
      {title ? <title>{title}</title> : null}
      <path d="M160.219 0C248.705 2.34906e-05 320.437 71.7324 320.438 160.219C320.437 248.705 248.705 320.437 160.219 320.438C71.7324 320.438 2.35732e-05 248.705 0 160.219C-1.21838e-05 71.7324 71.7324 1.1051e-05 160.219 0ZM170.219 90.417C164.031 86.8444 156.407 86.8445 150.219 90.417L104.769 116.658C98.5808 120.231 94.7686 126.833 94.7686 133.979V186.459C94.7686 193.604 98.5808 200.207 104.769 203.779L150.219 230.021C156.407 233.593 164.031 233.593 170.219 230.021L215.669 203.779C221.857 200.207 225.669 193.604 225.669 186.459V133.979C225.669 126.833 221.857 120.231 215.669 116.658L170.219 90.417Z" />
      <path d="M478.478 19.8457C484.286 9.71696 498.895 9.71695 504.703 19.8457L661.864 293.951C667.642 304.028 660.367 316.585 648.752 316.585H334.43C322.814 316.585 315.54 304.028 321.317 293.951L478.478 19.8457ZM498.147 145.475C495.244 140.41 487.939 140.41 485.035 145.475L436.547 230.043C433.658 235.081 437.295 241.359 443.102 241.359H540.079C545.887 241.359 549.524 235.081 546.636 230.043L498.147 145.475Z" />
      <path d="M977.327 12.2494C989.849 12.2494 1000 22.3997 1000 34.9213V278.912C1000 291.434 989.849 301.585 977.327 301.585H826.933C814.412 301.586 804.262 311.736 804.262 324.257V369.663C804.262 382.185 794.11 392.336 781.589 392.336H721.129C708.607 392.336 698.457 382.185 698.457 369.663V34.9213C698.457 22.3998 708.607 12.2496 721.129 12.2494H977.327ZM762.693 143.302C762.693 151.65 769.46 158.417 777.808 158.417H860.646C868.993 158.417 875.761 165.185 875.761 173.532V208.245C875.761 216.593 882.528 223.36 890.876 223.36H920.646C928.993 223.36 935.761 216.593 935.761 208.245V113.532C935.761 105.185 928.993 98.4174 920.646 98.4174H777.808C769.46 98.4174 762.693 105.185 762.693 113.532V143.302Z" />
    </svg>
  );
}

// ── Relay mark ───────────────────────────────────────────────────────────────
// The mark with a comet band running its outline, colour handing over at each
// corner (the "pulse relay" from the 2026-09-16 motion lab, round 5 pick Y2:
// comet tail, handover every SECOND corner). The
// band splits at the vertex it is crossing so the new hue grows out of the
// corner while the old one drains into it: it never stops and never goes dark.
// Hues use the chart series, so the relay follows the theme.
//
// Motion is for indication only: the band runs while `active` is true (hover
// on the brand, a working state) and fades out when it is not, leaving the
// plain mark. Honours prefers-reduced-motion by never starting.

const MARK_OUTLINE =
  "M159.186 7.59677C164.993 -2.53226 179.604 -2.53226 185.411 7.59677L342.572 281.702C348.349 291.779 341.075 304.335 329.459 304.335H15.138C3.52262 304.335 -3.75273 291.779 2.02468 281.702L159.186 7.59677Z";
const MARK_HOLE =
  "M178.855 133.226C175.951 128.161 168.646 128.161 165.742 133.226L117.254 217.794C114.366 222.832 118.003 229.11 123.811 229.11H220.786C226.594 229.11 230.232 222.832 227.343 217.794L178.855 133.226Z";
const RELAY_HUES = [1, 2, 3, 4, 5].map((n) => `hsl(var(--chart-${n}))`);
// The comet tail is RELAY_TAIL_STEPS short dashes behind the band, each a step
// dimmer, so the trailing edge fades instead of stopping hard (a dash cannot
// carry a gradient). Tail = RELAY_TAIL_LENGTH band lengths.
const RELAY_TAIL_STEPS = 12;
const RELAY_TAIL_LENGTH = 3.2; // 2x the original 1.6 (Corey, 2026-09-16)
const RELAY_TAIL_OPACITY = 0.55;
// Opacity of the mark itself while the relay runs (Corey, 2026-09-16: 35%).
// This is the HOVER treatment only; the setup wizard keeps its mark at full ink.
const RELAY_INK_OPACITY = 0.35;

export type OapMarkRelayProps = React.SVGProps<SVGSVGElement> & {
  title?: string;
  /** Run the relay. False parks the band and shows the plain mark. */
  active?: boolean;
  /** Milliseconds per lap of the outline. */
  lapMs?: number;
  /** Band length as a percentage of the perimeter. */
  band?: number;
  /** Band stroke width in mark units (viewBox is 345 wide): 12 ≈ 0.7px at 20px. */
  bandWidth?: number;
  /** Corners the head crosses before the hue advances. 2 = two edges per colour (Y2). */
  handoverEvery?: number;
};

export function prefersReducedMotion(): boolean {
  return typeof window !== "undefined" &&
    typeof window.matchMedia === "function"
    ? window.matchMedia("(prefers-reduced-motion: reduce)").matches
    : false;
}

function setDash(
  el: SVGPathElement | null,
  start: number,
  len: number,
  color: string,
) {
  if (!el) return;
  if (len <= 0.05) {
    el.style.opacity = "0";
    return;
  }
  el.style.opacity = "1";
  el.style.stroke = color;
  el.style.strokeDasharray = `${len} ${100 - len}`;
  el.style.strokeDashoffset = `${-start}`;
}

export function OapMarkRelay({
  className,
  title,
  active = false,
  lapMs = 2400,
  band = 12,
  bandWidth = 12,
  handoverEvery = 2,
  ...props
}: OapMarkRelayProps) {
  const segA = React.useRef<SVGPathElement>(null);
  const segB = React.useRef<SVGPathElement>(null);
  const segT = React.useRef<(SVGPathElement | null)[]>([]);
  const running = active && !prefersReducedMotion();

  React.useEffect(() => {
    if (!running) return;
    const t0 = performance.now();
    let raf = 0;
    const tick = (now: number) => {
      const lap = (now - t0) / lapMs; // laps travelled by the head
      const head = (lap * 100) % 100;
      const tail = (head - band + 100) % 100;
      const crossings = Math.floor(lap * 3); // vertices the head has crossed (apex is 0)
      const hueIndex = Math.floor(crossings / handoverEvery);
      const hueNew = RELAY_HUES[hueIndex % RELAY_HUES.length];
      const hueOld =
        RELAY_HUES[(hueIndex - 1 + RELAY_HUES.length) % RELAY_HUES.length];
      const lastCross = ((crossings / 3) * 100) % 100;
      const sinceCross = (head - lastCross + 100) % 100;
      const splitting = crossings % handoverEvery === 0 && sinceCross < band;
      if (splitting) {
        // Old hue behind the corner, new hue growing out beyond it.
        setDash(segA.current, tail, band - sinceCross, hueOld);
        setDash(segB.current, lastCross, sinceCross, hueNew);
      } else {
        setDash(segA.current, tail, band, hueNew);
        setDash(segB.current, 0, 0, hueNew);
      }
      const tailHue = splitting ? hueOld : hueNew;
      const step = (band * RELAY_TAIL_LENGTH) / RELAY_TAIL_STEPS;
      for (let k = 0; k < RELAY_TAIL_STEPS; k++) {
        const el = segT.current[k];
        setDash(el, (tail - step * (k + 1) + 100) % 100, step + 0.15, tailHue);
        if (el)
          el.style.opacity = (
            RELAY_TAIL_OPACITY * Math.pow(1 - k / RELAY_TAIL_STEPS, 1.6)
          ).toFixed(3);
      }
      raf = requestAnimationFrame(tick);
    };
    raf = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(raf);
  }, [running, lapMs, band, handoverEvery]);

  const segStyle: React.CSSProperties = { opacity: 0 };
  return (
    <svg
      viewBox="0 0 345 305"
      fill="currentColor"
      aria-hidden={title ? undefined : true}
      role={title ? "img" : undefined}
      className={cn("shrink-0 overflow-visible", className)}
      data-relay={running ? "on" : "off"}
      {...props}
    >
      {title ? <title>{title}</title> : null}
      {/* The mark itself dims to 35% while the band runs, so the relay reads
          as the subject and the mark as its track. */}
      <path
        d={MARK_OUTLINE + MARK_HOLE}
        style={{
          opacity: running ? RELAY_INK_OPACITY : 1,
          transition: "opacity 240ms ease-out",
        }}
        data-ink
      />
      <g
        fill="none"
        strokeWidth={bandWidth}
        strokeLinecap="round"
        strokeLinejoin="round"
        style={{
          opacity: running ? 1 : 0,
          transition: "opacity 240ms ease-out",
        }}
      >
        {Array.from({ length: RELAY_TAIL_STEPS }, (_, k) => (
          <path
            key={k}
            ref={(el) => {
              segT.current[k] = el;
            }}
            d={MARK_OUTLINE}
            pathLength={100}
            style={segStyle}
            strokeLinecap="butt"
            data-seg="tail"
          />
        ))}
        <path
          ref={segA}
          d={MARK_OUTLINE}
          pathLength={100}
          style={segStyle}
          data-seg="a"
        />
        <path
          ref={segB}
          d={MARK_OUTLINE}
          pathLength={100}
          style={segStyle}
          data-seg="b"
        />
      </g>
    </svg>
  );
}
