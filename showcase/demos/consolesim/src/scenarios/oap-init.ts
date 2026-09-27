import type { TermStory } from "../runtime/story";
import { bold, cyan, dim, gray, green, ok, step } from "../term/ansi";

// A scripted `oap init --local` run: the command typed at the prompt, then the
// build → install → seed → check → first-run-setup flow as colored output over
// time. Mirrors the phases in cmd/oap/internal/installcmd/init.go. All values
// are illustrative.
export function oapInit(): TermStory {
  return {
    prompt: `${cyan("❯")} `,
    cols: 94,
    rows: 30,
    intro: "oap init — one command from an empty cluster to a working OAP.",
    beats: [
      {
        id: "cmd",
        caption: "One command brings up the whole platform.",
        hold: 700,
        run: async (c) => {
          await c.prompt();
          await c.type("oap init --local");
          await c.enter();
          await c.wait(300);
        },
      },
      {
        id: "detect",
        caption: "It resolves the cluster kind — here, the local dev profile.",
        hold: 500,
        run: async (c) => {
          await c.line(dim("resolving cluster…"));
          await c.line(
            `${step} cluster kind: ${bold("local")} ${gray("(kind / Docker Desktop)")}`,
          );
          await c.line("");
        },
      },
      {
        id: "build",
        caption: "Builds the first-party images.",
        hold: 400,
        run: async (c) => {
          await c.line(`${step} ${bold("oap build all")}`);
          for (const img of [
            "operator",
            "runner",
            "channelsd",
            "webd",
            "authzd",
          ]) {
            await c.wait(150);
            await c.line(`  ${ok} ${img} ${gray("built")}`);
          }
          await c.line("");
        },
      },
      {
        id: "install",
        caption:
          "Installs the platform — a live checklist, each phase waited to Ready.",
        hold: 500,
        run: async (c) => {
          await c.line(`${step} ${bold("oap install")}`);
          for (const p of [
            "CRDs & operator",
            "Authorization (SpiceDB)",
            "Messaging bus",
            "Channels daemon",
            "Web server",
            "Memory store",
          ]) {
            await c.wait(210);
            await c.line(`  ${ok} ${p}`);
          }
          await c.line("");
        },
      },
      {
        id: "schema",
        caption:
          "Seeds the authorization schema everything is checked against.",
        hold: 400,
        run: async (c) => {
          await c.line(`${step} apply SpiceDB schema`);
          await c.wait(220);
          await c.line(`  ${ok} schema applied`);
          await c.line("");
        },
      },
      {
        id: "check",
        caption: "Health-checks every component until green.",
        hold: 500,
        run: async (c) => {
          await c.line(`${step} ${bold("oap check")}`);
          for (const comp of [
            "operator",
            "authzd",
            "channelsd",
            "webd",
            "spicedb",
            "memory",
          ]) {
            await c.wait(130);
            await c.line(`  ${ok} ${comp} ${gray("healthy")}`);
          }
          await c.line("");
        },
      },
      {
        id: "setup",
        caption:
          "Then walks you through first-run setup — identity, model, defaults.",
        hold: 700,
        run: async (c) => {
          await c.write(
            `${cyan("?")} Connect an identity provider? ${gray("[y/N]")} `,
          );
          await c.wait(450);
          await c.type("N", { cps: 6 });
          await c.enter();
          await c.write(
            `${cyan("?")} Default model ${gray("[provider/name]")} `,
          );
          await c.wait(300);
          await c.type("anthropic/claude-sonnet-5", { cps: 20 });
          await c.enter();
          await c.wait(200);
          await c.line(`  ${ok} default model registered`);
          await c.line(`  ${ok} secure defaults applied`);
          await c.line("");
        },
      },
      {
        id: "done",
        caption: "Ready — install an agent and talk to it.",
        hold: 1000,
        run: async (c) => {
          await c.line(green(bold("✓ agent-primitives is ready.")));
          await c.line(
            gray(
              "  next:  oap agent install <bundle>   ·   oap agent chat <class>",
            ),
          );
          await c.line("");
          await c.prompt();
        },
      },
    ],
  };
}
