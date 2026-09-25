// Same shape as chart.test.tsx: reads tokens.css off disk so the test measures
// the SHIPPED value rather than a copy of it, and runs in node so
// import.meta.url stays a real file:// URL.
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

const SURFACES = ["card", "surface-2", "surface-3"] as const;

for (const theme of THEMES)
  describe(`surface tokens (${theme})`, () => {
    const tokenLuminance = (name: string) => tokenLuminanceIn(theme, name);
    // WCAG ratios are not symmetric across the two grounds: the same perceptual
    // step (one design rung) yields a larger ratio near black than near white,
    // because the +0.05 term dominates at low luminance. Floors tuned on the dark
    // ramp are therefore too strict for light; these are the light equivalents
    // of the same rung steps, not a relaxation of the intent.
    const F =
      theme === "dark"
        ? { elevation: 1.12, hover: 1.35, active: 1.6, activeOverHover: 1.15 }
        : { elevation: 1.08, hover: 1.25, active: 1.5, activeOverHover: 1.12 };
    it("defines the surface ladder in tokens.css", () => {
      for (const name of [
        "surface-2",
        "surface-3",
        "surface-hover",
        "surface-active",
      ]) {
        expect(() => tokenLuminance(name)).not.toThrow();
      }
    });

    // The ladder must ASCEND. A surface that sits above another and is darker
    // than it inverts the depth cue, and on a near-black ground there is no
    // shadow budget left to argue the other way.
    // Dark rises toward light; light falls toward dark. Either way each step
    // moves AWAY from the ground in the same direction as the one before it.
    it("steps away from --background in one direction", () => {
      const ladder = ["background", ...SURFACES].map(tokenLuminance);
      for (let i = 1; i < ladder.length; i++) {
        if (theme === "dark")
          expect(
            ladder[i],
            `surface ${i} is not lighter than the one beneath it`,
          ).toBeGreaterThan(ladder[i - 1]);
        else
          expect(
            ladder[i],
            `surface ${i} is not darker than the one beneath it`,
          ).toBeLessThan(ladder[i - 1]);
      }
    });

    // 1.12 is the floor, not the target: today's tightest elevation pair sits at
    // ~1.18. Elevation is allowed to be quiet because a raised surface also
    // carries a border and a shadow; the floor only guarantees it is not relying
    // on them entirely.
    it("separates each elevation step from the one beneath it", () => {
      const ladder = [...SURFACES] as const;
      for (let i = 1; i < ladder.length; i++) {
        const r = contrastRatio(
          tokenLuminance(ladder[i]),
          tokenLuminance(ladder[i - 1]),
        );
        expect(
          r,
          `--${ladder[i]} vs --${ladder[i - 1]} is too close to read as a separate surface`,
        ).toBeGreaterThanOrEqual(F.elevation);
      }
    });

    // --background and --card are real design rungs (stone-975 and stone-900)
    // and the step between them is the ramp's own: 1.13:1, just over the 1.12
    // elevation floor above but not by enough to lean on. That floor governs surfaces WE placed; this step is
    // not ours to tune, and a card on the page ground also carries a border. The
    // assertion is a drift guard on the direction and rough size of the step,
    // not a claim that it meets the elevation bar.
    it("keeps --card a small step off --background", () => {
      const r = contrastRatio(
        tokenLuminance("card"),
        tokenLuminance("background"),
      );
      expect(r, "card/background step has collapsed").toBeGreaterThanOrEqual(
        theme === "dark" ? 1.08 : 1.03,
      );
    });

    // Interaction steps harder than elevation, and this is the regression the
    // file exists for. The shipped hover before these tokens was `bg-muted/50`
    // over --card: an opacity modifier on a dark ground, which composites TOWARD
    // the background and landed at 1.06:1 — a hover state that could not change a
    // pixel a viewer would notice. `bg-secondary/80` on a card was worse at
    // exactly 1.00:1, because --secondary and --card are the same value.
    //
    // 1.35 is the floor. The nav's own hover, which demonstrably reads, sits at
    // 2.24:1; that is louder than a table row wants, so --surface-hover is tuned
    // to 1.45 rather than to the nav.
    it("makes hover and active unmistakable against --card", () => {
      const card = tokenLuminance("card");
      expect(
        contrastRatio(tokenLuminance("surface-hover"), card),
      ).toBeGreaterThanOrEqual(F.hover);
      expect(
        contrastRatio(tokenLuminance("surface-active"), card),
      ).toBeGreaterThanOrEqual(F.active);
    });

    it("keeps active a clear step beyond hover", () => {
      const r = contrastRatio(
        tokenLuminance("surface-active"),
        tokenLuminance("surface-hover"),
      );
      expect(
        r,
        "pressed is not distinguishable from hover",
      ).toBeGreaterThanOrEqual(F.activeOverHover);
    });

    // Body text has to survive on every surface, not only on --card. A hover that
    // lightens the row while the text stays put is how a 4.5:1 pair quietly
    // becomes a 3.9:1 pair.
    it("keeps --foreground readable on every surface", () => {
      const fg = tokenLuminance("foreground");
      for (const name of [...SURFACES, "surface-hover", "surface-active"]) {
        expect(
          contrastRatio(fg, tokenLuminance(name)),
          `--foreground on --${name}`,
        ).toBeGreaterThanOrEqual(4.5);
      }
    });
  });
