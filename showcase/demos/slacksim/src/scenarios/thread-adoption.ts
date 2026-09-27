import { newScenario } from "../store/scenario";
import type { Story } from "../runtime/story";

// Thread adoption: two people are already discussing an incident in a thread.
// One @-mentions the bot, which is summoned INTO the existing thread — it
// backfills the prior discussion as context and grants the thread's members
// participant standing, then posts a transparency join notice. Models the Slack
// kind's adoption path (pkg/channels/channelkinds/slack: history.go backfill +
// listener.go formatJoinNotice / formatCollectiveOwnershipNotice). All
// identifiers are fabricated.

const ME = "U_OWNER"; // jordan
const SAM = "U6";
const RILEY = "U8";
const AGENT = "U7"; // opsbot
const INC = "C1";
const T = "1756700000.000000";

export function threadAdoption(): Story {
  const s = newScenario()
    .workspace("Acme Robotics", { glyph: "A", accent: "#4a154b" })
    .at("2026-08-30T14:45:00Z")
    .user(ME, "jordan")
    .user(SAM, "sam")
    .user(RILEY, "riley")
    .bot(AGENT, "opsbot", { badge: "AGENT" })
    .me(ME)
    .channel(INC, "incidents", { starred: true });

  // A real discussion, before the bot is anywhere near it.
  s.message(INC, SAM, {
    text: "Checkout was throwing 5xx for ~40m starting 14:05. Rolled back the payments deploy and it recovered.",
    ts: T,
  });
  s.reply(INC, T, RILEY, {
    text: "Confirmed — error rate is back to baseline. Root cause looks like the new timeout config.",
    ts: "1756700100.000000",
  });
  s.reply(INC, T, SAM, {
    text: "Agreed. Someone should write this up before the retro.",
    ts: "1756700200.000000",
  });

  // Someone @-mentions the bot INTO the existing thread.
  s.reply(INC, T, RILEY, {
    text: "<@U7> can you draft an incident summary from this thread?",
    ts: "1756700300.000000",
  });

  const scenario = s.open(INC).openThread(T).build();

  return {
    scenario,
    intro:
      "Thread adoption: @-mention the bot into a thread that’s already going.",
    beats: [
      {
        id: "join",
        caption:
          "@-mentioned into a thread that’s already underway, opsbot *adopts* it: the prior discussion becomes its context, and the people in it become participants.",
        hold: 1600,
        run: (c) => {
          c.postReply(INC, T, AGENT, {
            text:
              "👋 I’ve joined this thread. <@U6> and <@U8> can talk to me here by @-mentioning me — and because " +
              "everyone in <#C1|incidents> shares ownership of me, either of you can approve or deny what I do.",
          });
        },
      },
      {
        id: "summary",
        caption:
          "It answers from what’s already in the thread — the rollback, the confirmation, the root cause — not from a blank slate.",
        hold: 1600,
        run: (c) => {
          c.postReply(INC, T, AGENT, {
            text:
              "Draft incident summary:\n" +
              "• *Impact*: checkout 5xx for ~40m from 14:05.\n" +
              "• *Trigger*: the payments deploy; *root cause*: the new timeout config.\n" +
              "• *Resolution*: rolled back the deploy — error rate back to baseline (confirmed by <@U8>).\n" +
              "Want me to open a follow-up to fix the timeout config?",
          });
        },
      },
    ],
  };
}
