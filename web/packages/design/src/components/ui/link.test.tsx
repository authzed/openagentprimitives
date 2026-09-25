// Same shape as surface.test.tsx and chart.test.tsx: reads tokens.css off disk
// so the test measures the SHIPPED value rather than a copy of it, and runs in
// node so import.meta.url stays a real file:// URL.
// @vitest-environment node
import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const tokens = readFileSync(
  fileURLToPath(new URL("../../tokens.css", import.meta.url)),
  "utf8",
);

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

// Every surface a link can land on. --surface-active is last because it is the
// lightest, and so the binding constraint on a resting link colour.
const SURFACES = [
  "card",
  "surface-2",
  "surface-3",
  "surface-hover",
  "surface-active",
] as const;

for (const theme of THEMES)
  describe(`link tokens (${theme})`, () => {
    const tokenLuminance = (name: string) => tokenLuminanceIn(theme, name);
    it("defines --link in tokens.css and no raised rung", () => {
      expect(() => tokenLuminance("link")).not.toThrow();
      // Neutral links need no surface override; if someone reintroduces a raised
      // rung, this test is where they must argue for it.
      expect(tokens).not.toMatch(/--link-raised\s*:/);
    });

    // Links are one rung below --foreground so they read as text first. They
    // must still be distinguishable from body copy without the underline, and
    // must stay a separate role from --primary (ink buttons) so a link never
    // looks like a button label.
    it("sits just below --foreground and apart from --primary", () => {
      const link = tokenLuminance("link");
      if (theme === "dark")
        expect(link).toBeLessThan(tokenLuminance("foreground"));
      else expect(link).toBeGreaterThan(tokenLuminance("foreground"));
      expect(
        contrastRatio(link, tokenLuminance("foreground")),
      ).toBeGreaterThanOrEqual(1.1);
      expect(
        contrastRatio(link, tokenLuminance("primary")),
      ).toBeGreaterThanOrEqual(1.15);
    });

    it("clears 4.5:1 on every surface, --surface-active included", () => {
      const link = tokenLuminance("link");
      for (const name of SURFACES) {
        expect(
          contrastRatio(link, tokenLuminance(name)),
          `--link on --${name}`,
        ).toBeGreaterThanOrEqual(4.5);
      }
    });
  });
