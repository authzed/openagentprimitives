import { newScenario } from "../store/scenario";
import type { Story } from "../runtime/story";
import type { Block } from "../blockkit/types";
import approval from "../fixtures/blockkit/approval.json";
import plan from "../fixtures/blockkit/plan.json";
import message from "../fixtures/blockkit/message.json";

// A story that renders the REAL Block Kit OAP emits — the fixtures captured by
// `mage blocks:capture` from OAP's actual slack sender — so a demo can show
// exactly what OAP posts (custom container + plan blocks included).
export function realBlocks(): Story {
  const P = "1756500000.000000";
  const s = newScenario()
    .workspace("Acme Robotics", { glyph: "A", accent: "#4a154b" })
    .at("2026-08-28T18:00:00Z")
    .user("U1", "jordan")
    .bot("U2", "srebot", { badge: "AGENT" })
    .me("U1")
    .channel("C1", "deploys", { starred: true })
    .channel("C3", "general");

  s.message("C1", "U1", {
    text: "<@U2> roll out `hotfix-1.4.2` to production.",
    ts: P,
  });
  s.reply("C1", P, "U2", { blocks: plan as Block[], ts: "1756500100.000000" });
  s.reply("C1", P, "U2", {
    blocks: approval as Block[],
    ts: "1756500200.000000",
  });
  s.reply("C1", P, "U2", {
    blocks: message as Block[],
    ts: "1756500300.000000",
  });

  return { scenario: s.open("C1").openThread(P).build() };
}
