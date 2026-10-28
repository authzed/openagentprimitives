import { newScenario } from "../store/scenario";
import type { Story } from "../runtime/story";
import type { Block } from "../blockkit/types";
import type { ArtifactView } from "../store/types";

// The Claude Design case study (site/content/docs/case-study-claude-design.mdx):
// messagingbot scopes a brief, grounds itself in the brand repos through a
// read-only gh tool, delivers an HTML artifact, and revises it from notes the
// requester pins onto the rendered page in the live viewer.
//
// The artifact card and the viewer are authored (an artifact offer is a
// button, not a captured interaction card; the viewer is a mock of
// pkg/web/webui/artifactview). The artifact body is authored HTML standing in
// for what the agent renders. All identifiers — Acme Robotics, its repos, its
// people, its product — are fabricated.

const ME = "U_OWNER"; // jordan
const AGENT = "U7"; // messagingbot
const MKT = "C1";
const REQ_TS = "1756700000.000000";
const CARD_TS = "1756700600.000000";

// Acme's (fabricated) brand: Signal Orange on warm paper, set in Georgia for
// display and the system sans for body copy.
const BRAND = {
  orange: "#ff5a1f",
  ink: "#14161a",
  paper: "#faf7f2",
  muted: "#5d6470",
};

function hero(opts: {
  title: string;
  sub: string;
  cta: string;
  ctaStyle: string;
}): string {
  return `
<section style="background:${BRAND.paper};color:${BRAND.ink};padding:72px 64px 64px;min-height:100%;box-sizing:border-box;font-family:-apple-system,'Segoe UI',Inter,system-ui,sans-serif">
  <div style="display:flex;align-items:center;gap:10px;margin-bottom:56px">
    <span style="display:inline-grid;place-items:center;width:30px;height:30px;border-radius:8px;background:${BRAND.ink};color:${BRAND.paper};font-weight:800">A</span>
    <span style="font-weight:700;letter-spacing:.01em">Acme Robotics</span>
  </div>
  <p id="hero-eyebrow" style="margin:0 0 16px;font-size:12px;font-weight:700;letter-spacing:.14em;text-transform:uppercase;color:${BRAND.orange}">New · Fleet Autopilot</p>
  <h1 id="hero-title" style="margin:0 0 18px;max-width:15ch;font:600 54px/1.06 Georgia,'Times New Roman',serif;letter-spacing:-.01em">${opts.title}</h1>
  <p id="hero-sub" style="margin:0 0 32px;max-width:46ch;font-size:18px;line-height:1.55;color:${BRAND.muted}">${opts.sub}</p>
  <div style="display:flex;align-items:center;gap:20px">
    <a id="hero-cta" style="${opts.ctaStyle}">${opts.cta}</a>
    <span style="font-size:14px;color:${BRAND.muted}">Live in 6 weeks · no new hardware</span>
  </div>
  <div style="display:grid;grid-template-columns:repeat(3,1fr);gap:16px;margin-top:64px;max-width:760px">
    ${[
      ["99.4%", "picks on time through peak"],
      ["38%", "fewer robot idle minutes"],
      ["1 console", "for every site and shift"],
    ]
      .map(
        ([k, v]) =>
          `<div style="padding:16px 18px;background:#fff;border:1px solid #ebe5da;border-radius:12px"><div style="font:600 24px Georgia,serif">${k}</div><div style="font-size:13px;color:${BRAND.muted}">${v}</div></div>`,
      )
      .join("")}
  </div>
</section>`;
}

const REV1 = hero({
  title: "Smarter robots for a smarter warehouse",
  sub: "Fleet Autopilot uses AI to coordinate your robots so your operation can run more efficiently.",
  cta: "Learn more",
  ctaStyle: `display:inline-block;padding:13px 22px;border-radius:999px;border:1.5px solid ${BRAND.ink};color:${BRAND.ink};font-weight:600;text-decoration:none`,
});

const REV2 = hero({
  title: "Every pick, on time. Even at peak.",
  sub: "Fleet Autopilot re-plans every robot, every second, around the orders that are actually late — so your floor holds its SLA on the worst day of the year.",
  cta: "Book a demo",
  ctaStyle: `display:inline-block;padding:14px 24px;border-radius:999px;background:${BRAND.orange};color:#fff;font-weight:700;text-decoration:none;box-shadow:0 6px 18px rgba(255,90,31,.32)`,
});

const VIEW: ArtifactView = {
  title: "Fleet Autopilot — launch hero",
  description: "Homepage hero for warehouse ops leads",
  revision: 1,
  revisions: [{ n: 1, note: "First pass from acme/design tokens" }],
  html: REV1,
  annotations: [],
};

const card = (context: string): Block[] => [
  {
    type: "section",
    text: {
      type: "mrkdwn",
      text: "🎨  *Fleet Autopilot — launch hero*  ·  HTML artifact",
    },
  },
  { type: "context", elements: [{ type: "mrkdwn", text: context }] },
  {
    type: "actions",
    block_id: "artifact_actions",
    elements: [
      {
        type: "button",
        text: { type: "plain_text", text: "View live", emoji: false },
        action_id: "open_artifact",
        style: "primary",
      },
    ],
  },
];

export function messagingbotDesign(): Story {
  const s = newScenario()
    .workspace("Acme Robotics", { glyph: "A", accent: "#4a154b" })
    .at("2026-10-20T16:00:00Z")
    .user(ME, "jordan")
    .bot(AGENT, "messagingbot", { badge: "AGENT" })
    .me(ME)
    .channel(MKT, "marketing", { starred: true });

  s.message(MKT, ME, {
    text: "<@U7> we need a homepage hero for the Fleet Autopilot launch",
    ts: REQ_TS,
  });

  const scenario = s.open(MKT).openThread(REQ_TS).build();

  return {
    scenario,
    intro:
      "Claude Design, rebuilt from OAP parts. messagingbot turns a brief into an on-brand page, and takes feedback pinned right onto the design.",
    beats: [
      {
        id: "scope",
        caption:
          "Like a good designer, messagingbot scopes the brief before it draws anything: who it's for, the one message, and what the button should do.",
        hold: 900,
        run: (c) => {
          c.postReply(MKT, REQ_TS, AGENT, {
            text: "Happy to. Three quick questions first:\n1. Who's it for — ops leads on the floor, or the execs who sign?\n2. The one thing they should remember?\n3. What should the button do?\nAnd which repos hold your design system and messaging?",
          });
        },
      },
      {
        id: "answer",
        caption:
          "Jordan answers, and points it at the brand repos. Those are the only repositories this install lets it read.",
        hold: 700,
        run: (c) => {
          c.postReply(MKT, REQ_TS, ME, {
            text: "ops leads at 3PLs. peak-season reliability. book a demo. brand is in acme/design + acme/messaging",
          });
        },
      },
      {
        id: "ground",
        caption:
          "It posts a plan, loads its design skill, and reads the real colour tokens and approved copy, through a GitHub tool that can only read from approved repos.",
        hold: 1100,
        run: (c) => {
          c.postReply(MKT, REQ_TS, AGENT, {
            text: "Plan:\n• Load the `frontend-design` skill\n• Read `acme/design` — tokens/color.json, tokens/type.json, components/button.md\n• Read `acme/messaging` — pillars.md, voice.md\n• Draft the hero, critique it, deliver",
          });
          c.setStatus(REQ_TS, "reading acme/messaging/pillars.md…");
        },
      },
      {
        id: "deliver",
        caption:
          "The design comes back as an artifact: versioned, stored durably, and rendered inert, so its HTML can't run a single script.",
        hold: 900,
        run: (c) => {
          c.setStatus(REQ_TS, undefined);
          c.postReply(MKT, REQ_TS, AGENT, {
            blocks: card("revision 1  ·  versioned  ·  rendered inert"),
            ts: CARD_TS,
          });
        },
      },
      {
        id: "open",
        caption:
          "View live opens it in OAP's artifact viewer: the rendered page, its revision history, and a live link back to the session.",
        hold: 900,
        clickAction: "open_artifact",
        run: (c) => c.openArtifact(VIEW),
      },
      {
        id: "pin-title",
        caption:
          "Now the Claude Design move. Jordan clicks the headline and pins a note to it: it's off-message.",
        hold: 1000,
        run: (c) =>
          c.annotateArtifact({
            index: 1,
            targetId: "hero-title",
            label: "Smarter robots for a smarter…",
            comment:
              "Generic. Lead with our peak-season pillar — the outcome, not the AI.",
            intent: "change",
            author: "jordan",
          }),
      },
      {
        id: "pin-cta",
        caption: "And another on the button: wrong words, and the wrong colour.",
        hold: 800,
        run: (c) =>
          c.annotateArtifact({
            index: 2,
            targetId: "hero-cta",
            label: "Learn more",
            comment: "Should be “Book a demo”, as a primary button in Signal Orange.",
            intent: "change",
            author: "jordan",
          }),
      },
      {
        id: "send",
        caption:
          "Both notes go back as one structured batch. The server checks that Jordan may view this exact artifact before the agent sees a word of it.",
        hold: 900,
        clickAction: "send_annotations",
        run: (c) => c.sendAnnotations("messagingbot"),
      },
      {
        id: "revise",
        caption:
          "messagingbot revises the same artifact. Revision 2 fixes both notes and says so in its change note, and revision 1 is still there, byte for byte.",
        hold: 1300,
        run: (c) =>
          c.reviseArtifact(
            REV2,
            "1: headline from acme/messaging pillars.md · 2: primary CTA, Signal Orange",
          ),
      },
      {
        id: "back",
        caption:
          "No new platform feature made this possible. It's an agent definition, a read-only tool, two skills, and the artifact viewer OAP already ships.",
        hold: 1400,
        run: (c) => {
          c.closeArtifact();
          c.editMessage(MKT, CARD_TS, {
            blocks: card(
              "revision 2  ·  `latest` → v2  ·  revision 1 kept",
            ),
          });
          c.postReply(MKT, REQ_TS, AGENT, {
            text: "Revision 2 is up:\n1. Headline now leads with “Every pick, on time” — the peak-season pillar in `acme/messaging/pillars.md`.\n2. CTA is “Book a demo”, the primary button from `acme/design/components/button.md`.",
          });
        },
      },
    ],
  };
}
