import { newScenario } from "../store/scenario";
import type { Story } from "../runtime/story";
import type { Block } from "../blockkit/types";

// Memory: an agent recalls what it needs from an authorized, searchable store,
// then produces a durable, versioned ARTIFACT and delivers it — attached to its
// reply, not just mentioned. The artifact card is authored here (an artifact
// offer is a link/button, not a captured interaction card). All identifiers are
// fabricated.

const ME = "U_OWNER";
const SAM = "U6";
const AGENT = "U7"; // opsbot
const OPS = "C1";
const REQ_TS = "1756700000.000000";
const CARD_TS = "1756700400.000000";

// Authored artifact-delivery card: a durable, versioned, inert output offered
// back with an Open button.
const artifact: Block[] = [
  {
    type: "section",
    text: {
      type: "mrkdwn",
      text: "📄  *Q3 Incident Review*  ·  rendered report",
    },
  },
  {
    type: "context",
    elements: [
      {
        type: "mrkdwn",
        text: "revision 2  ·  durable & versioned  ·  rendered inert",
      },
    ],
  },
  {
    type: "actions",
    block_id: "artifact_actions",
    elements: [
      {
        type: "button",
        text: { type: "plain_text", text: "Open report", emoji: false },
        action_id: "open_artifact",
        style: "primary",
      },
    ],
  },
];

export function memoryArtifact(): Story {
  const s = newScenario()
    .workspace("Acme Robotics", { glyph: "A", accent: "#4a154b" })
    .at("2026-08-30T17:00:00Z")
    .user(ME, "jordan")
    .user(SAM, "sam")
    .bot(AGENT, "opsbot", { badge: "AGENT" })
    .me(ME)
    .channel(OPS, "ops", { starred: true });

  s.message(OPS, SAM, {
    text: "<@U7> pull together a Q3 incident review from what we logged",
    ts: REQ_TS,
  });

  const scenario = s.open(OPS).openThread(REQ_TS).build();

  return {
    scenario,
    intro:
      "Memory: recall from an authorized store, then produce and deliver a durable artifact.",
    beats: [
      {
        id: "recall",
        caption:
          "opsbot recalls what it needs — an authorized, searchable store, not the whole chat history stuffed into the prompt.",
        hold: 1300,
        run: (c) => {
          c.setStatus(REQ_TS, "searching memory…");
          c.postReply(OPS, REQ_TS, AGENT, {
            text: "Found 7 incidents logged in Q3 across payments and search. Compiling the review now…",
          });
        },
      },
      {
        id: "deliver",
        caption:
          "It produces a durable, versioned *artifact* and delivers it — attached to the reply, not just mentioned. Producing isn’t the same as delivering.",
        hold: 1600,
        run: (c) => {
          c.setStatus(REQ_TS, undefined);
          c.postMessage(OPS, AGENT, { blocks: artifact, ts: CARD_TS });
        },
      },
      {
        id: "inert",
        caption:
          "The report is rendered *inert* — sanitized, served from an isolated origin, its scripts can’t run — and every step that got here is in a signed, tamper-evident log.",
        hold: 1500,
        run: (c) => {
          c.postReply(OPS, REQ_TS, AGENT, {
            text: "Delivered *Q3 Incident Review* (v2). It’s versioned, so I can revise it in place — and the whole run is in the audit log if you want to verify it.",
          });
        },
      },
    ],
  };
}
