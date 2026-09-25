import * as React from "react";
import { ResponsiveContainer, Tooltip } from "recharts";
import { cn } from "../../lib/utils";

// CHART_SERIES_VARS is the ordered series palette. Consumers index into it by
// series position rather than hardcoding a color, so a token change in
// tokens.css propagates everywhere and no chart can drift off the design
// system. Five entries is deliberate: past five series a chart should be
// re-thought, not given a sixth color.
//
// Every token stores a bare HSL triplet (e.g. `195 76% 70%`), matching every
// other entry in tokens.css, so it must be wrapped in hsl(...) — same as
// ChartTooltip below, styles.css, and tailwind-preset.ts. An unwrapped
// `var(--chart-1)` is not a valid CSS <color>; substituted into an SVG
// stroke/fill it fails at computed-value time and falls back to `none`,
// rendering an invisible line with no error.
export const CHART_SERIES_VARS = [
  "hsl(var(--chart-1))",
  "hsl(var(--chart-2))",
  "hsl(var(--chart-3))",
  "hsl(var(--chart-4))",
  "hsl(var(--chart-5))",
] as const;

// seriesColor wraps the palette so an out-of-range index cycles rather than
// rendering an undefined stroke.
export function seriesColor(index: number): string {
  return CHART_SERIES_VARS[index % CHART_SERIES_VARS.length];
}

// ChartContainer gives every chart the same responsive box and typography.
// Recharts needs a concretely-sized ancestor or it collapses to zero in an
// auto-height flex parent and renders nothing — that's why height is an
// explicit prop rather than something inherited, with a sane default so most
// callers never have to think about it.
export function ChartContainer({
  height = 240,
  className,
  children,
}: {
  height?: number;
  className?: string;
  children: React.ReactElement;
}) {
  return (
    <div className={cn("w-full text-xs", className)} style={{ height }}>
      <ResponsiveContainer width="100%" height="100%">
        {children}
      </ResponsiveContainer>
    </div>
  );
}

// ChartTooltip is Recharts' Tooltip pinned to the design system's surface
// colors, so a tooltip never renders as an unstyled white box on the dark shell.
export function ChartTooltip(props: React.ComponentProps<typeof Tooltip>) {
  return (
    <Tooltip
      cursor={{ stroke: "hsl(var(--border))" }}
      contentStyle={{
        background: "hsl(var(--popover))",
        border: "1px solid hsl(var(--border))",
        borderRadius: "var(--radius)",
        color: "hsl(var(--popover-foreground))",
        fontSize: "0.75rem",
      }}
      {...props}
    />
  );
}
