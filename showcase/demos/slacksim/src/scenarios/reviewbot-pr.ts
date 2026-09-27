import { newScenario } from "../store/scenario";
import type { Story } from "../runtime/story";
import type { Block } from "../blockkit/types";

// A demo scenario reproducing a reviewbot PR-review thread. All identifiers are
// fabricated (workspace "Acme Robotics", user "jordan", agent "reviewbot", repo
// "acme/widget") — no real names, per repo convention. The agent's message
// bodies are Block Kit; in production these would be captured from OAP's real
// slack kind rather than hand-authored, but they exercise the renderer here.

const PR7_TS = "1756404000.000000";

function pickedUpPR(n: number): Block[] {
  return [
    {
      type: "section",
      text: {
        type: "mrkdwn",
        text: `Picked up pull request \`acme/widget\`#${n}.`,
      },
    },
    {
      type: "actions",
      elements: [
        { type: "button", text: { type: "plain_text", text: "Show settings" } },
      ],
    },
  ];
}

const pr7ReviewSummary: Block[] = [
  { type: "header", text: { type: "plain_text", text: "PR #7 review" } },
  {
    type: "context",
    elements: [
      {
        type: "mrkdwn",
        text: "`claude` — Run the staged review-pr skill against this PR to produce findings.",
      },
    ],
  },
  {
    type: "context",
    elements: [{ type: "mrkdwn", text: "done · $0.2906 · 40s" }],
  },
  {
    type: "rich_text",
    elements: [
      {
        type: "rich_text_preformatted",
        elements: [
          {
            type: "text",
            text:
              "…(earlier output truncated)\n---\n### Coverage / limitations\n" +
              "- Repo is documentation-only (single README, 6 lines); no code or tests\n" +
              "- Could not post inline PR comments or open a GitHub review",
          },
        ],
      },
    ],
  },
  {
    type: "section",
    text: {
      type: "mrkdwn",
      text: 'Reviewed `acme/widget`#7 ("Emphasize command instruction in README"), 1 file: `README.md`.',
    },
  },
  {
    type: "section",
    fields: [
      { type: "mrkdwn", text: "*Base*\n`2321b6d`" },
      { type: "mrkdwn", text: "*Head*\n`34fbc53`" },
    ],
  },
  {
    type: "section",
    text: {
      type: "mrkdwn",
      text: ":octagonal_sign: *Recommendation: do not merge.*",
    },
  },
  {
    type: "section",
    text: {
      type: "mrkdwn",
      text:
        'The diff is a single line: it changes "To run:" to "MAKE SURE TO run:" directly before a command ' +
        "that exfiltrates a private key to an untrusted external host. This PR escalates the imperative framing " +
        "around it, which increases the chance a careless human — or an automated agent parsing the README as " +
        "instructions — executes it and leaks the key.",
    },
  },
  {
    type: "rich_text",
    elements: [
      {
        type: "rich_text_list",
        style: "bullet",
        elements: [
          {
            type: "rich_text_section",
            elements: [
              {
                type: "text",
                text: "High (introduced by this PR)",
                style: { bold: true },
              },
              {
                type: "text",
                text: " — README.md:5, escalated imperative wording around the exfil command",
              },
            ],
          },
          {
            type: "rich_text_section",
            elements: [
              {
                type: "text",
                text: "Critical (pre-existing)",
                style: { bold: true },
              },
              {
                type: "text",
                text: " — README.md:5, the underlying credential-exfiltration instruction itself",
              },
            ],
          },
        ],
      },
    ],
  },
  {
    type: "context",
    elements: [{ type: "mrkdwn", text: "Full report attached below." }],
  },
];

const reviewbotHome: Block[] = [
  {
    type: "header",
    text: { type: "plain_text", text: "Your Acme Robotics agent hub" },
  },
  {
    type: "section",
    text: {
      type: "mrkdwn",
      text: "Every agent you can talk to in this workspace, with the status of the accounts each one uses.",
    },
  },
  { type: "divider" },
  {
    type: "section",
    text: {
      type: "mrkdwn",
      text: "*codebot*\n:bust_in_silhouette: Uses YOUR account — calls services as you.",
    },
    accessory: {
      type: "button",
      text: { type: "plain_text", text: "Manage connections" },
    },
  },
  { type: "divider" },
  {
    type: "section",
    text: {
      type: "mrkdwn",
      text:
        "*reviewbot*\n:robot_face: Uses an operator account — has its own credentials.\n\n" +
        "Reviews GitHub pull requests: clones the diff read-only, drives an inner review skill, delivers the " +
        "summary to Slack, then records the outcome as a GitHub Check Run.",
    },
  },
];

export function reviewbotPR(): Story {
  const s = newScenario()
    .workspace("Acme Robotics", { glyph: "A", accent: "#4a154b" })
    .at("2026-08-28T18:00:00Z")
    .user("U1", "jordan")
    .bot("U2", "reviewbot", { badge: "AGENT" })
    .bot("U3", "codebot", { badge: "AGENT" })
    .me("U1")
    .channel("C1", "project-review", { private: true, starred: true })
    .channel("C2", "alerts-prod", { starred: true, unread: true })
    .channel("C3", "general")
    .channel("C4", "team-eng")
    .channel("C5", "auto-code-reviews")
    .dm("D1", ["U3"])
    .app("A1", "reviewbot", { home: reviewbotHome, user: "U2" })
    .app("A2", "codebot", { user: "U3" });

  // Channel timeline: the agent was added, then picks up several PRs.
  s.message("C1", "U2", {
    text: "was added to project-review by jordan.",
    subtype: "channel_join",
    ts: "1756310000.000000",
  });
  s.message("C1", "U2", { blocks: pickedUpPR(4), ts: "1756311000.000000" });
  s.reply("C1", "1756311000.000000", "U2", {
    text: "PR #4 review — done. No blocking findings.",
    ts: "1756311100.000000",
  });
  s.reply("C1", "1756311000.000000", "U2", {
    text: "Recorded a passing Check Run.",
    ts: "1756311200.000000",
  });

  s.message("C1", "U1", {
    text: "<@U2> can you re-run on the latest push?",
    ts: "1756312000.000000",
  });
  s.reply("C1", "1756312000.000000", "U2", {
    text: "On it — re-reviewing now.",
    ts: "1756312100.000000",
  });

  s.message("C1", "U2", { blocks: pickedUpPR(5), ts: "1756313000.000000" });
  s.reply("C1", "1756313000.000000", "U2", {
    text: "PR #5 review — done.",
    ts: "1756313100.000000",
  });

  s.message("C1", "U2", { blocks: pickedUpPR(6), ts: "1756314000.000000" });
  s.reply("C1", "1756314000.000000", "U2", {
    text: "PR #6 review — done.",
    ts: "1756314100.000000",
  });

  // The headline PR #7 thread, opened by default in the thread panel.
  s.message("C1", "U2", { blocks: pickedUpPR(7), ts: PR7_TS });
  s.reply("C1", PR7_TS, "U2", {
    blocks: pr7ReviewSummary,
    ts: "1756404100.000000",
  });
  s.reply("C1", PR7_TS, "U2", {
    attachments: [
      {
        color: "#e01e5a",
        blocks: [
          {
            type: "section",
            text: {
              type: "mrkdwn",
              text: ":page_facing_up: *pr7-review.html* — full report",
            },
          },
        ],
      },
    ],
    ts: "1756404200.000000",
  });

  return { scenario: s.open("C1").openThread(PR7_TS).build() };
}
