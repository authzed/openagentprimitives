import { newScenario } from "../store/scenario";
import type { Story } from "../runtime/story";

// Scripted product-manager example. The agent reads fabricated GitHub and
// Linear records, then reports alignment without writing to either service.
const PRODUCT = "C1";
const BOT = "U2";
const ROOT = "1756900000.000000";

export function pmAgentDemo(): Story {
  const s = newScenario()
    .workspace("Acme Robotics", { glyph: "A", accent: "#4a154b" })
    .at("2026-09-02T15:00:00Z")
    .user("U1", "jordan")
    .bot(BOT, "pm-bot", { badge: "AGENT" })
    .me("U1")
    .channel(PRODUCT, "product", { starred: true });
  s.message(PRODUCT, "U1", {
    text: "<@U2> how do our open PRs and Linear issues align with our product goals this week?",
    ts: ROOT,
  });

  return {
    scenario: s.open(PRODUCT).openThread(ROOT).build(),
    intro: "pm-bot: a read-only view of work in flight against product goals.",
    beats: [
      {
        id: "read",
        caption:
          "pm-bot reads only the GitHub repos it was granted and the linked Linear issues.",
        hold: 1000,
        run: (c) => {
          c.setStatus(ROOT, "reading GitHub and Linear");
          c.postReply(PRODUCT, ROOT, BOT, {
            text: "Reading `acme/web` and `acme/api`, then matching open PRs to Linear issues. I won't change either system.",
          });
        },
      },
      {
        id: "summary",
        caption:
          "It groups work by goal, showing what is on track and what needs attention.",
        hold: 1300,
        run: (c) => {
          c.setStatus(ROOT, undefined);
          c.postReply(PRODUCT, ROOT, BOT, {
            text: "*Weekly product alignment*\n✅ *Faster checkout:* `acme/web` PR #18 and LIN-241 address the payment retry flow.\n⚠️ *Self-serve onboarding:* LIN-255 is blocked; no open PR covers the first-run guide.\n➡️ Suggested next step: assign LIN-255 and link its implementation PR.\nNo changes were made in GitHub or Linear.",
          });
        },
      },
    ],
  };
}
