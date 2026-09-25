import { newScenario } from '../store/scenario'
import type { Story } from '../runtime/story'
import type { Block } from '../blockkit/types'
import queued from '../fixtures/blockkit/queued-messages.json'

// Mid-turn reply: a message sent while the agent is working is queued, not
// dropped or auto-interrupting — with a one-click "Interrupt & Send Now". The
// card is OAP's real queued_messages Block Kit (captured by `mage blocks:capture`).
// All identifiers are fabricated.

const ME = 'U_OWNER'
const AGENT = 'U7' // researchbot
const TEAM = 'C1'
const REQ_TS = '1756700000.000000'
const CARD_TS = '1756700400.000000'
const blocks = queued as Block[]

export function queuedReply(): Story {
  const s = newScenario()
    .workspace('Acme Robotics', { glyph: 'A', accent: '#4a154b' })
    .at('2026-08-30T13:30:00Z')
    .user(ME, 'jordan')
    .bot(AGENT, 'researchbot', { badge: 'AGENT' })
    .me(ME)
    .channel(TEAM, 'research', { starred: true })

  s.message(TEAM, ME, { text: '<@U7> pull the latest numbers into the deck', ts: REQ_TS })
  s.reply(TEAM, REQ_TS, AGENT, { text: 'On it — compiling the figures now…', ts: '1756700100.000000' })

  const scenario = s.open(TEAM).openThread(REQ_TS).build()

  return {
    scenario,
    intro: 'Mid-turn: a message sent while the agent is working is queued — with a one-click interrupt.',
    beats: [
      {
        id: 'card',
        caption:
          'You add a message while researchbot is mid-turn. It isn’t dropped or spliced in — it’s queued, with a one-click “Interrupt & Send Now”.',
        hold: 1000,
        run: (c) => {
          c.postReply(TEAM, REQ_TS, ME, { text: 'actually — make it Q3 only' })
          c.postMessage(TEAM, AGENT, { blocks, ts: CARD_TS })
        },
      },
    ],
  }
}
