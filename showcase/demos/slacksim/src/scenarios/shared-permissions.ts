import { newScenario } from "../store/scenario";
import type { Story } from "../runtime/story";
import type { Block } from "../blockkit/types";
import contactShare from "../fixtures/blockkit/contact-share.json";
import contactShareResolved from "../fixtures/blockkit/contact-share-resolved.json";

// Shared permissions: a company's data is owned by that company's owner, and
// another person can't read it without the owner's approval. Sam asks the CRM
// agent for Circldot's contacts; Circldot is owned by Jordan; the agent routes an
// approval to JORDAN (the resource owner, not the session owner) and only shares
// once Jordan approves.
//
// The approval card is OAP's real tool_approval Block Kit (captured by
// `mage blocks:capture`) — the one whose `Resources` names crm_company:circldot
// and whose DecideResourceOwners policy routes it to crm_company#owner. The
// owner id is U_OWNER so the fixture's decider mention resolves to jordan. All
// identifiers are fabricated. Models examples/agent-hubspot-companies.

const OWNER = "U_OWNER"; // jordan — Circldot's account owner / "me"
const SAM = "U6"; // the teammate asking
const AGENT = "U7"; // crmbot (a CRM/HubSpot company agent)
const SALES = "C1";
const DM_JORDAN = "D1";
const REQ_TS = "1756700000.000000";
const CARD_TS = "1756700300.000000";

const shareBlocks = contactShare as Block[];
const shareResolvedBlocks = contactShareResolved as Block[];

export function sharedPermissions(): Story {
  const s = newScenario()
    .workspace("Acme Robotics", { glyph: "A", accent: "#4a154b" })
    .at("2026-08-30T16:00:00Z")
    .user(OWNER, "jordan")
    .user(SAM, "sam")
    .bot(AGENT, "crmbot", { badge: "AGENT" })
    .me(OWNER)
    .channel("C1", "sales", { starred: true })
    .channel("C2", "general")
    .dm("D1", [AGENT]) // jordan ↔ crmbot
    .app("A1", "crmbot", { user: AGENT });

  // Sam asks the CRM agent for a company's contacts; the agent explains it needs
  // the owner's approval first.
  s.message(SALES, SAM, {
    text: "<@U7> can you pull the current contacts for *Circldot* for the QBR deck?",
    ts: REQ_TS,
  });
  s.reply(SALES, REQ_TS, AGENT, {
    text: "Those contacts belong to <@U_OWNER> (Circldot's account owner) — I've asked them to approve before sharing. One sec.",
    ts: "1756700100.000000",
  });

  const scenario = s.open(SALES).openThread(REQ_TS).build();

  return {
    scenario,
    intro:
      "Shared permissions: a company's owner approves before its contacts are shared.",
    beats: [
      {
        id: "dm",
        caption:
          "The contacts belong to Circldot's owner, so crmbot DMs Jordan — the owner — to approve, not the person who asked.",
        hold: 1100,
        run: (c) => {
          c.postMessage(DM_JORDAN, AGENT, { blocks: shareBlocks, ts: CARD_TS });
          c.setStatus(REQ_TS, "waiting for Circldot's owner to approve");
          c.switchChannel(DM_JORDAN);
        },
      },
      {
        id: "approve",
        caption:
          "Jordan owns Circldot, so Jordan is the one who can approve — one click, and the card resolves.",
        hold: 900,
        run: (c) => {
          c.editMessage(DM_JORDAN, CARD_TS, { blocks: shareResolvedBlocks });
          c.postMessage(DM_JORDAN, AGENT, {
            text: "Thanks — sharing Circldot’s contacts in <#C1|sales> now.",
          });
        },
      },
      {
        id: "return",
        caption:
          "Only now does crmbot return the contacts — to Sam, back in the channel where the request began.",
        hold: 1100,
        run: (c) => {
          c.switchChannel(SALES);
          c.openThread(REQ_TS);
          c.setStatus(REQ_TS, undefined);
          c.postReply(SALES, REQ_TS, AGENT, {
            text:
              "Here are *Circldot*’s current contacts, <@U6> — approved by <@U_OWNER>:\n" +
              "• *Dana Okafor* — VP Engineering — `dana@circldot.example`\n" +
              "• *Priya Raman* — Head of Data — `priya@circldot.example`\n" +
              "• *Marco Silva* — Procurement — `marco@circldot.example`",
          });
        },
      },
    ],
  };
}
