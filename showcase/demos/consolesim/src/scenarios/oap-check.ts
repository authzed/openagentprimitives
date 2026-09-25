import type { TermStory } from "../runtime/story";
import { bold, cyan, gray, green, ok, warnGlyph } from "../term/ansi";

// A scripted `oap check` run: the per-component health readout (✓ / ⚠) OAP prints
// when you ask whether the platform is healthy. Illustrative values.
export function oapCheck(): TermStory {
  return {
    prompt: `${cyan("❯")} `,
    cols: 84,
    rows: 18,
    intro: "oap check — is every component healthy?",
    beats: [
      {
        id: "cmd",
        caption: "Ask whether the platform is healthy.",
        hold: 500,
        run: async (c) => {
          await c.prompt();
          await c.type("oap check");
          await c.enter();
          await c.wait(300);
        },
      },
      {
        id: "components",
        caption:
          "Each component reports for itself — ✓ healthy, ⚠ degraded, ✗ down.",
        hold: 600,
        run: async (c) => {
          const rows: [string, string, string][] = [
            [ok, "operator", "reconciling"],
            [ok, "authzd", "ready"],
            [ok, "spicedb", "serving"],
            [ok, "channelsd", "connected"],
            [ok, "webd", "serving :8080"],
            [warnGlyph, "memory", "graphiti not configured (optional)"],
          ];
          for (const [glyph, name, note] of rows) {
            await c.wait(150);
            await c.line(`  ${glyph} ${bold(name.padEnd(11))} ${gray(note)}`);
          }
          await c.line("");
        },
      },
      {
        id: "summary",
        caption:
          "A green check means it’s actually configured, not just running.",
        hold: 900,
        run: async (c) => {
          await c.line(
            green("healthy") + gray(" — 5 ready, 1 optional not configured"),
          );
          await c.line("");
          await c.prompt();
        },
      },
    ],
  };
}
