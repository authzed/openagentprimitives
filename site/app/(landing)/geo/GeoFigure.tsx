"use client";

// The typed boundary over figures.js (hand-authored geometry, plain JS). The
// figure's svg() string is deterministic, so the server and the client render
// the same markup; animation starts only after mount.
//
// All live figures share ONE 40ms interval, started when the first animating
// figure mounts and cleared when the last unmounts. A figure's clock advances
// only while it is on screen (IntersectionObserver, 80px margin), the document
// is visible, and the caller has not paused it. prefers-reduced-motion leaves
// every figure on its t0 frame.
import { useEffect, useMemo, useRef } from "react";
import { FIGS as RAW_FIGS } from "./figures";

export const GEO_NAMES = [
  "union",
  "define",
  "gate",
  "broker",
  "lens",
  "channels",
  "memory",
  "peraction",
  "injection",
  "leakage",
  "delegation",
  "revoke",
  "pods",
  "depth",
] as const;

export type GeoName = (typeof GEO_NAMES)[number];

type Figure = {
  svg(): string;
  anim?(svg: SVGSVGElement, t: number): void;
  t0?: number;
};

export const FIGS = RAW_FIGS as unknown as Record<GeoName, Figure>;

type Live = {
  svg: SVGSVGElement;
  anim: (svg: SVGSVGElement, t: number) => void;
  t: number;
  visible: boolean;
  paused: { current: boolean };
};

const live = new Set<Live>();
let timer: ReturnType<typeof setInterval> | undefined;
let last = 0;

function tick() {
  const now = performance.now();
  const dt = Math.min(0.1, (now - last) / 1000);
  last = now;
  if (document.hidden) return;
  for (const e of live) {
    if (!e.visible || e.paused.current) continue;
    e.t += dt;
    e.anim(e.svg, e.t);
  }
}

function register(e: Live) {
  live.add(e);
  if (timer === undefined) {
    last = performance.now();
    timer = setInterval(tick, 40);
  }
}

function unregister(e: Live) {
  live.delete(e);
  if (live.size === 0 && timer !== undefined) {
    clearInterval(timer);
    timer = undefined;
  }
}

function prefersReducedMotion(): boolean {
  return (
    typeof matchMedia === "function" &&
    matchMedia("(prefers-reduced-motion: reduce)").matches
  );
}

export function GeoFigure({
  name,
  label,
  className,
  paused = false,
  hue,
}: {
  name: GeoName;
  label: string;
  className?: string;
  paused?: boolean;
  /** Starting relay hue (0-4); figures read it from data-hue on this div. */
  hue?: number;
}): React.JSX.Element {
  const ref = useRef<HTMLDivElement>(null);
  const pausedRef = useRef(paused);
  const html = useMemo(() => ({ __html: FIGS[name].svg() }), [name]);

  useEffect(() => {
    pausedRef.current = paused;
  }, [paused]);

  useEffect(() => {
    const fig = FIGS[name];
    const svg = ref.current?.querySelector("svg");
    if (!svg || !fig.anim) return;
    const entry: Live = {
      svg,
      anim: fig.anim,
      t: fig.t0 ?? 0,
      visible: false,
      paused: pausedRef,
    };
    fig.anim(svg, entry.t);
    if (prefersReducedMotion() || typeof IntersectionObserver !== "function")
      return;
    const io = new IntersectionObserver(
      (es) => {
        entry.visible = es[es.length - 1].isIntersecting;
      },
      { rootMargin: "80px" },
    );
    io.observe(svg);
    register(entry);
    return () => {
      io.disconnect();
      unregister(entry);
    };
  }, [name]);

  return (
    <div
      ref={ref}
      className={className ? `geo ${className}` : "geo"}
      role="img"
      aria-label={label}
      data-geo={name}
      data-hue={hue}
      dangerouslySetInnerHTML={html}
    />
  );
}
