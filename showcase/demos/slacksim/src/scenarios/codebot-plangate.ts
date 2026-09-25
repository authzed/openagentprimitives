import { newScenario } from "../store/scenario";
import type { Story } from "../runtime/story";
import type { Block } from "../blockkit/types";
import planApproval from "../fixtures/blockkit/plan-approval.json";
import planApprovalResolved from "../fixtures/blockkit/plan-approval-resolved.json";

// codebot (a coding agent) is asked to fix a test and open a PR on a SPECIFIC
// repo. It proposes a multi-phase plan; the plan gate raises ONE approval that
// covers the whole plan; approving it slots that repo so only it is authorized.
//
// The plan-gate approval card is the REAL Block Kit OAP emits (category
// plan_phase), captured by `mage blocks:capture` — phases with blast-radius-toned
// permission lines and the "reaches acme/widget" resource line in each phase.
// The owner id is U_OWNER so the fixture's `<@U_OWNER>` mention resolves to
// jordan. All identifiers are fabricated.

const OWNER = "U_OWNER"; // jordan, session owner / "me"
const AGENT = "U3"; // codebot
const ENG = "C1";
const REQ_TS = "1756600000.000000";
const PLAN_CARD_TS = "1756600300.000000";

const planBlocks = planApproval as Block[];
const planResolvedBlocks = planApprovalResolved as Block[];

export function codebotPlangate(): Story {
  const s = newScenario()
    .workspace("Acme Robotics", { glyph: "A", accent: "#4a154b" })
    .at("2026-08-29T15:00:00Z")
    .user(OWNER, "jordan")
    .bot(AGENT, "codebot", { badge: "AGENT" })
    .me(OWNER)
    .channel("C1", "eng", { starred: true })
    .channel("C2", "incidents")
    .channel("C3", "general")
    .dm("D1", [AGENT])
    .app("A1", "codebot", { user: AGENT });

  s.message(ENG, OWNER, {
    text: "<@U3> the payments flaky test is back — can you fix it in `acme/widget` and open a PR?",
    ts: REQ_TS,
  });
  s.reply(ENG, REQ_TS, AGENT, {
    text: "On it. Here's my plan — I'll need a quick approval before I touch the repo.",
    ts: "1756600100.000000",
  });

  const scenario = s.open(ENG).openThread(REQ_TS).build();

  return {
    scenario,
    intro:
      "Plan gating: one approval covers a whole plan — and slots the exact repo it names.",
    beats: [
      {
        id: "gate",
        caption:
          "codebot proposes a multi-phase plan; the plan gate asks for ONE approval that covers all of it.",
        hold: 1100,
        run: (c) => {
          c.postReply(ENG, REQ_TS, AGENT, {
            blocks: planBlocks,
            ts: PLAN_CARD_TS,
          });
          c.setStatus(REQ_TS, "waiting for an owner to approve the plan");
        },
      },
      {
        id: "approve",
        caption:
          "Jordan approves the whole plan in one click — the card resolves in place.",
        hold: 900,
        run: (c) => {
          c.editMessage(ENG, PLAN_CARD_TS, { blocks: planResolvedBlocks });
          c.setStatus(REQ_TS, undefined);
          c.postReply(ENG, REQ_TS, AGENT, {
            text: "Approved by <@U_OWNER> — running the plan now.",
          });
        },
      },
      {
        id: "run",
        caption:
          "Inside the approved plan, the individual steps run without prompting again.",
        hold: 900,
        run: (c) => {
          c.setStatus(REQ_TS, "cloning acme/widget and applying the fix…");
          c.postReply(ENG, REQ_TS, AGENT, {
            text: "Cloned `acme/widget`, fixed the flaky test, and the payments suite is green. Pushing the branch…",
          });
        },
      },
      {
        id: "pr",
        caption:
          "codebot opens the pull request and reports back with the link.",
        hold: 1000,
        run: (c) => {
          c.setStatus(REQ_TS, undefined);
          c.postReply(ENG, REQ_TS, AGENT, {
            text: "Opened <https://github.com/acme/widget/pull/128|PR #128> on `acme/widget`: *Fix flaky payments test*.",
          });
        },
      },
      {
        id: "reask",
        caption:
          "The approval was slotted to acme/widget only — a different repo needs a fresh one.",
        hold: 1100,
        run: (c) => {
          c.postReply(ENG, REQ_TS, OWNER, {
            text: "nice — can you do the same on `acme/other`?",
          });
          c.postReply(ENG, REQ_TS, AGENT, {
            text: "`acme/other` is a different repository, so this approval doesn’t cover it — I scoped it to `acme/widget`. I’ll draft a plan and ask for a fresh approval for `acme/other`.",
          });
        },
      },
    ],
  };
}
