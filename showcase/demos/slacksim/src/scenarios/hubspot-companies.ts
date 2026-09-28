import { newScenario } from "../store/scenario";
import type { Story } from "../runtime/story";
import type { Block } from "../blockkit/types";
import contactShare from "../fixtures/blockkit/contact-share.json";
import contactShareResolved from "../fixtures/blockkit/contact-share-resolved.json";

// Scripted HubSpot example. Company names, people and scores are fabricated;
// the owner-approval card is captured from OAP's real Slack renderer.
const SALES = "C1";
const DM = "D1";
const REQUEST = "1756700000.000000";
const CARD = "1756700300.000000";
const OWNER = "U_OWNER";
const SAM = "U6";
const BOT = "U7";

export function hubspotCompanies(): Story {
  const s = newScenario()
    .workspace("Acme Robotics", { glyph: "A", accent: "#4a154b" })
    .at("2026-08-31T09:00:00Z")
    .user(OWNER, "jordan")
    .user(SAM, "sam")
    .bot(BOT, "hubspot-companies", { badge: "AGENT" })
    .me(OWNER)
    .channel(SALES, "sales", { starred: true })
    .dm(DM, [BOT]);
  return {
    scenario: s.open(SALES).build(),
    intro:
      "HubSpot CRM: a scheduled company digest, with owner approval for private contacts.",
    beats: [
      {
        id: "digest",
        caption:
          "Each week, the agent finds newly created companies with strong fit scores and tags their owners.",
        hold: 1000,
        run: (c) =>
          c.postMessage(SALES, BOT, {
            text: "*New companies this week*\n• *Circldot* — fit score 91, managed by <@U_OWNER>\n• *Northstar Labs* — fit score 85, managed by <@U_OWNER>\n2 companies met the reporting threshold.",
          }),
      },
      {
        id: "request",
        caption:
          "Sam asks for Circldot's contacts. Those records belong to Jordan, so the agent pauses.",
        hold: 900,
        run: (c) => {
          c.postMessage(SALES, SAM, {
            text: "<@U7> can you pull Circldot's contacts for the QBR?",
            ts: REQUEST,
          });
          c.openThread(REQUEST);
          c.postReply(SALES, REQUEST, BOT, {
            text: "Circldot's contacts need approval from <@U_OWNER>, the account owner. I've asked Jordan now.",
          });
          c.setStatus(REQUEST, "waiting for Circldot's owner");
        },
      },
      {
        id: "owner-approval",
        caption:
          "The approval goes to Jordan in a DM, with the exact company and data being shared.",
        hold: 1100,
        run: (c) => {
          c.postMessage(DM, BOT, { blocks: contactShare as Block[], ts: CARD });
          c.switchChannel(DM);
        },
      },
      {
        id: "approve",
        caption:
          "Jordan approves. The card resolves in place before any contacts appear in the sales thread.",
        hold: 900,
        run: (c) =>
          c.editMessage(DM, CARD, { blocks: contactShareResolved as Block[] }),
      },
      {
        id: "contacts",
        caption:
          "Now the agent shares Circldot's contacts with Sam, and records whose approval allowed it.",
        hold: 1100,
        run: (c) => {
          c.switchChannel(SALES);
          c.openThread(REQUEST);
          c.setStatus(REQUEST, undefined);
          c.postReply(SALES, REQUEST, BOT, {
            text: "Circldot contacts for <@U6>, approved by <@U_OWNER>:\n• Dana Okafor — VP Engineering\n• Priya Raman — Head of Data",
          });
        },
      },
    ],
  };
}
