import { newScenario } from "../store/scenario";
import type { Story } from "../runtime/story";
import type { Block } from "../blockkit/types";
import approval from "../fixtures/blockkit/approval.json";
import approvalResolved from "../fixtures/blockkit/approval-resolved.json";

// Safe tools: an agent can only do what was written down, and an action that
// LEAVES the session — an external tool call — asks a human first. The card is
// OAP's real tool_approval Block Kit (captured by `mage blocks:capture`): a deploy
// to production, computed from the tool call rather than the agent's pitch. All
// identifiers are fabricated.

const ME = "U_OWNER"; // the operator who approves / "me"
const DEV = "U6"; // teammate who asked
const AGENT = "U7"; // shipbot
const OPS = "C1";
const REQ_TS = "1756700000.000000";
const CARD_TS = "1756700300.000000";

const approvalBlocks = approval as Block[];
const approvalResolvedBlocks = approvalResolved as Block[];

export function safeTools(): Story {
  const s = newScenario()
    .workspace("Acme Robotics", { glyph: "A", accent: "#4a154b" })
    .at("2026-08-30T15:00:00Z")
    .user(ME, "jordan")
    .user(DEV, "sam")
    .bot(AGENT, "shipbot", { badge: "AGENT" })
    .me(ME)
    .channel(OPS, "ops", { starred: true })
    .app("A1", "shipbot", { user: AGENT });

  s.message(OPS, DEV, {
    text: "<@U7> ship `hotfix-1.4.2` to prod once the checks pass",
    ts: REQ_TS,
  });
  s.reply(OPS, REQ_TS, AGENT, {
    text: "Running the pre-deploy checks now…",
    ts: "1756700100.000000",
  });

  const scenario = s.open(OPS).openThread(REQ_TS).build();

  return {
    scenario,
    intro:
      "Safe tools: read-only steps run freely, but an external action asks a human first.",
    beats: [
      {
        id: "checks",
        caption:
          "Read-only tools — reading the build status, the diff — run without asking. Nothing has left the session yet.",
        hold: 1000,
        run: (c) => {
          c.postReply(OPS, REQ_TS, AGENT, {
            text: "✅ Build is green and the smoke suite passed. Ready to deploy.",
          });
        },
      },
      {
        id: "ask",
        caption:
          "The deploy is an *external* action — it leaves the session and can’t be undone — so shipbot asks first. The card is computed from the tool call, not the agent’s words.",
        hold: 1500,
        run: (c) => {
          c.postMessage(OPS, AGENT, { blocks: approvalBlocks, ts: CARD_TS });
          c.setStatus(REQ_TS, "waiting for approval to deploy");
        },
      },
      {
        id: "approve",
        caption:
          "One click approves exactly this call — deploy hotfix-1.4.2 to prod-us-east — and nothing else.",
        hold: 900,
        run: (c) => {
          c.editMessage(OPS, CARD_TS, { blocks: approvalResolvedBlocks });
          c.setStatus(REQ_TS, "deploying");
        },
      },
      {
        id: "done",
        caption:
          "Approved, so the tool runs — as an argv array in a locked-down, non-root sandbox, never through a shell.",
        hold: 1200,
        run: (c) => {
          c.setStatus(REQ_TS, undefined);
          c.postReply(OPS, REQ_TS, AGENT, {
            text: "✅ `hotfix-1.4.2` is live on prod-us-east — 12 pods restarted, smoke suite green.",
          });
        },
      },
    ],
  };
}
