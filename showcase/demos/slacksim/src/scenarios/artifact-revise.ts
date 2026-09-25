import { newScenario } from '../store/scenario'
import type { Story } from '../runtime/story'
import type { Block } from '../blockkit/types'

// Artifacts: a durable, versioned output is delivered, then REVISED IN PLACE — a
// new immutable revision advances the head and `latest`, while the link keeps
// pointing at the report. The artifact card is authored here (an artifact offer
// is a link/button, not a captured interaction card). All identifiers are
// fabricated.

const ME = 'U_OWNER'
const SAM = 'U6'
const AGENT = 'U7' // opsbot
const OPS = 'C1'
const REQ_TS = '1756700000.000000'
const CARD_TS = '1756700400.000000'

const card = (context: string): Block[] => [
  { type: 'section', text: { type: 'mrkdwn', text: '📄  *Q3 Incident Review*  ·  rendered report' } },
  { type: 'context', elements: [{ type: 'mrkdwn', text: context }] },
  {
    type: 'actions',
    block_id: 'artifact_actions',
    elements: [
      {
        type: 'button',
        text: { type: 'plain_text', text: 'Open report', emoji: false },
        action_id: 'open_artifact',
        style: 'primary',
      },
    ],
  },
]

const cardV2 = card('revision 2  ·  durable & versioned  ·  rendered inert')
const cardV3 = card('revision 3  ·  updated: added the Sept 14 payments outage  ·  `latest` → v3')

export function artifactRevise(): Story {
  const s = newScenario()
    .workspace('Acme Robotics', { glyph: 'A', accent: '#4a154b' })
    .at('2026-08-30T17:00:00Z')
    .user(ME, 'jordan')
    .user(SAM, 'sam')
    .bot(AGENT, 'opsbot', { badge: 'AGENT' })
    .me(ME)
    .channel(OPS, 'ops', { starred: true })

  s.message(OPS, SAM, { text: '<@U7> pull together a Q3 incident review from what we logged', ts: REQ_TS })

  const scenario = s.open(OPS).openThread(REQ_TS).build()

  return {
    scenario,
    intro: 'Artifacts: a durable, versioned output — delivered, then revised in place.',
    beats: [
      {
        id: 'deliver',
        caption:
          'opsbot produces a durable, versioned *artifact* and delivers it — attached to the reply, not just mentioned.',
        hold: 1300,
        run: (c) => {
          c.postMessage(OPS, AGENT, { blocks: cardV2, ts: CARD_TS })
          c.postReply(OPS, REQ_TS, AGENT, { text: 'Delivered *Q3 Incident Review* (revision 2).' })
        },
      },
      {
        id: 'ask',
        caption: 'You ask for a change — add the outage everyone forgot.',
        hold: 900,
        run: (c) => {
          c.postReply(OPS, REQ_TS, SAM, { text: 'can you add the Sept 14 payments outage?' })
        },
      },
      {
        id: 'revise',
        caption:
          'It revises *in place* — a new immutable revision advances the head and moves `latest` to v3, while the link keeps pointing at the report. Nothing anyone already saw was mutated.',
        hold: 1600,
        run: (c) => {
          c.editMessage(OPS, CARD_TS, { blocks: cardV3 })
          c.postReply(OPS, REQ_TS, AGENT, {
            text: 'Updated in place — now *revision 3*. `latest` resolves to v3; the earlier revision is still addressable.',
          })
        },
      },
    ],
  }
}
