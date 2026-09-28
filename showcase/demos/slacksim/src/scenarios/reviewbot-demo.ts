import { newScenario } from "../store/scenario";
import type { Story } from "../runtime/story";

// Scripted example of examples/reviewbot. GitHub and Slack accounts are fake;
// the sequence follows the bundle's pull-request webhook → read-only review → Slack → Check Run flow.
const REVIEWS = "C1";
const BOT = "U2";
const ROOT = "1756800000.000000";

export function reviewbotDemo(): Story {
  const s = newScenario()
    .workspace("Acme Robotics", { glyph: "A", accent: "#4a154b" })
    .at("2026-09-01T15:00:00Z")
    .user("U1", "jordan")
    .bot(BOT, "reviewbot", { badge: "AGENT" })
    .me("U1")
    .channel(REVIEWS, "auto-code-reviews", { starred: true });
  s.message(REVIEWS, BOT, {
    text: "GitHub pull request updated: `acme/widget` PR #42, head `8f27ca1`. Starting a review.",
    ts: ROOT,
  });

  return {
    scenario: s.open(REVIEWS).openThread(ROOT).build(),
    intro: "reviewbot: a pull-request event starts a read-only review.",
    beats: [
      {
        id: "read-diff",
        caption:
          "A verified pull-request event starts the session. reviewbot checks out the diff read-only and selects its review skill.",
        hold: 1200,
        run: (c) => {
          c.setStatus(ROOT, "reading PR #42 diff");
          c.postReply(REVIEWS, ROOT, BOT, {
            text: "Reviewing 3 changed files with the staged `review-pr` skill. Repository access is read-only.",
          });
        },
      },
      {
        id: "findings",
        caption:
          "The review finds a missing permission check and shows a report reference beside the Slack summary.",
        hold: 1300,
        run: (c) => {
          c.postReply(REVIEWS, ROOT, BOT, {
            text: "*PR #42 review — do not merge yet*\nHigh: `payments/handler.go` skips the account-owner check on retries. The new test covers the happy path but not a retry by a different user.",
            attachments: [
              {
                color: "#e01e5a",
                blocks: [
                  {
                    type: "section",
                    text: {
                      type: "mrkdwn",
                      text: ":page_facing_up: *pr42-review.html* — report reference",
                    },
                  },
                ],
              },
            ],
          });
        },
      },
      {
        id: "check-run",
        caption:
          "Only after delivery does reviewbot conclude the GitHub Check Run on the exact head commit.",
        hold: 1200,
        run: (c) => {
          c.setStatus(ROOT, undefined);
          c.postReply(REVIEWS, ROOT, BOT, {
            text: "Check Run concluded: action required on `8f27ca1`. The review did not modify the repository or comment on the PR.",
          });
        },
      },
    ],
  };
}
