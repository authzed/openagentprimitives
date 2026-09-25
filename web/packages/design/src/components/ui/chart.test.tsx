// This file only reads tokens.css and static exports — no DOM is needed —
// and jsdom (the project default) forces Vite's browser transform, which
// rewrites `import.meta.url` to resolve against jsdom's mocked
// `self.location` instead of the real file path, breaking fileURLToPath
// below. Node env keeps import.meta.url as the real file:// URL.
// @vitest-environment node
import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { CHART_SERIES_VARS } from "./chart";

const tokens = readFileSync(
  fileURLToPath(new URL("../../tokens.css", import.meta.url)),
  "utf8",
);

// hslToRgb / relativeLuminance / contrastRatio are the WCAG 2.x definitions,
// inlined because they are ~15 lines and the package has no color dependency
// worth adding for a single test.
function hslToRgb(h: number, s: number, l: number): [number, number, number] {
  s /= 100;
  l /= 100;
  const k = (n: number) => (n + h / 30) % 12;
  const a = s * Math.min(l, 1 - l);
  const f = (n: number) =>
    l - a * Math.max(-1, Math.min(k(n) - 3, Math.min(9 - k(n), 1)));
  return [f(0), f(8), f(4)];
}

function relativeLuminance([r, g, b]: [number, number, number]): number {
  const lin = (v: number) =>
    v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
  return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
}

const contrastRatio = (a: number, b: number): number =>
  (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);

// Both theme blocks live in tokens.css: `:root { … }` is dark (the default the
// server stamps as data-theme="dark") and `:root[data-theme="light"] { … }` is
// light. Each block is measured on its own, so a test runs once per theme and a
// rung that is right in one and wrong in the other fails by name.
type Theme = "dark" | "light";
const THEMES: readonly Theme[] = ["dark", "light"];
function themeBlock(theme: Theme): string {
  const start =
    theme === "dark"
      ? tokens.indexOf(":root {")
      : tokens.indexOf(':root[data-theme="light"] {');
  if (start < 0) throw new Error(`tokens.css has no ${theme} block`);
  return tokens.slice(start, tokens.indexOf("}", start));
}
function tokenLuminanceIn(theme: Theme, name: string): number {
  const m = themeBlock(theme).match(
    new RegExp(`--${name}:\\s*([\\d.]+)\\s+([\\d.]+)%\\s+([\\d.]+)%\\s*;`),
  );
  if (!m)
    throw new Error(
      `tokens.css ${theme} block does not define --${name} as an HSL triplet`,
    );
  return relativeLuminance(hslToRgb(Number(m[1]), Number(m[2]), Number(m[3])));
}

for (const theme of THEMES)
  describe(`chart series tokens (${theme})`, () => {
    const tokenLuminance = (name: string) => tokenLuminanceIn(theme, name);
    it("defines five distinct chart variables in tokens.css", () => {
      const found = [
        ...themeBlock(theme).matchAll(/--chart-([1-5]):\s*([^;]+);/g),
      ];
      expect(found).toHaveLength(5);

      const values = found.map((m) => m[2].trim());
      expect(new Set(values).size).toBe(5);
    });

    it("uses five distinct chart colors", () => {
      const hex = (name: string) => {
        const m = themeBlock(theme).match(
          new RegExp(
            `--${name}:\\s*([\\d.]+)\\s+([\\d.]+)%\\s+([\\d.]+)%\\s*;`,
          ),
        );
        if (!m) throw new Error(`tokens.css does not define --${name}`);
        const [r, g, b] = hslToRgb(Number(m[1]), Number(m[2]), Number(m[3]));
        return (
          "#" +
          [r, g, b]
            .map((v) =>
              Math.round(v * 255)
                .toString(16)
                .padStart(2, "0"),
            )
            .join("")
        );
      };
      expect(new Set([1, 2, 3, 4, 5].map((i) => hex(`chart-${i}`))).size).toBe(
        5,
      );
    });

    // Legibility is measured against --card, not --background: every chart in
    // the admin UI renders inside a Card, so the page ground is never what a
    // series is drawn on. A chart line or bar is a graphical object under WCAG
    // 1.4.11, whose threshold is 3:1.
    it("keeps every series legible against --card", () => {
      const card = tokenLuminance("card");
      for (const i of [1, 2, 3, 4, 5]) {
        expect(
          contrastRatio(tokenLuminance(`chart-${i}`), card),
          `--chart-${i} does not clear 3:1 against --card`,
        ).toBeGreaterThanOrEqual(3);
      }
    });

    it("exposes exactly those variables as the series palette", () => {
      // Wrapped in hsl(...): tokens.css stores a bare HSL triplet per entry, and
      // an unwrapped var(--chart-N) is not a valid CSS <color> — a stroke/fill
      // using it fails silently at computed-value time instead of erroring.
      expect(CHART_SERIES_VARS).toEqual([
        "hsl(var(--chart-1))",
        "hsl(var(--chart-2))",
        "hsl(var(--chart-3))",
        "hsl(var(--chart-4))",
        "hsl(var(--chart-5))",
      ]);
    });

    it("pins the hsl(var(--chart-N)) wrapping itself, not just today's strings", () => {
      for (const entry of CHART_SERIES_VARS) {
        expect(entry).toMatch(/^hsl\(var\(--chart-[1-5]\)\)$/);
      }
    });
  });
