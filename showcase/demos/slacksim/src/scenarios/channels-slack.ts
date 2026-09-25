import { newScenario } from '../store/scenario'
import type { Story } from '../runtime/story'
import type { Block } from '../blockkit/types'

// Channels: an agent is reached where the team already works. An @mention opens a
// real, durable thread; the App Home tab is the at-a-glance hub for what the agent
// is and which accounts it uses. The thread messages are plain Slack; the Home
// view is authored here to mirror OAP's real "hub" (a card per agent with its
// identity mode and connection status). All identifiers are fabricated.

const ME = 'U_OWNER' // "me"
const DEV = 'U6'
const AGENT = 'U7' // helpbot
const SUPPORT = 'C1'
const APP = 'A1'
const REQ_TS = '1756700000.000000'

// Authored App Home ("Your helpbot hub"), matching the real app_home_view shape:
// header + intro context + a card per bound agent showing identity mode and
// per-service connection status + a Manage-connections action.
const home: Block[] = [
  { type: 'header', text: { type: 'plain_text', text: 'Your helpbot hub', emoji: false } },
  {
    type: 'context',
    elements: [{ type: 'mrkdwn', text: 'Reach helpbot with an @mention in any channel, or message it here.' }],
  },
  { type: 'divider' },
  { type: 'section', text: { type: 'mrkdwn', text: '*helpbot*   ·   Uses an operator account' } },
  {
    type: 'context',
    elements: [
      { type: 'mrkdwn', text: '✓ Zendesk linked' },
      { type: 'mrkdwn', text: '✓ Knowledge base linked' },
    ],
  },
  {
    type: 'actions',
    block_id: 'home_actions',
    elements: [
      {
        type: 'button',
        text: { type: 'plain_text', text: 'Manage connections', emoji: false },
        action_id: 'manage',
        style: 'primary',
      },
    ],
  },
]

export function channelsSlack(): Story {
  const s = newScenario()
    .workspace('Acme Robotics', { glyph: 'A', accent: '#4a154b' })
    .at('2026-08-30T14:00:00Z')
    .user(ME, 'jordan')
    .user(DEV, 'sam')
    .bot(AGENT, 'helpbot', { badge: 'AGENT' })
    .me(ME)
    .channel(SUPPORT, 'support', { starred: true })
    .app(APP, 'helpbot', { user: AGENT, home })

  s.message(SUPPORT, DEV, {
    text: '<@U7> what’s our refund window for annual plans?',
    ts: REQ_TS,
  })

  const scenario = s.open(SUPPORT).openThread(REQ_TS).build()

  return {
    scenario,
    intro: 'Channels: reached where the team already works — a real thread, and an App Home hub.',
    beats: [
      {
        id: 'reply',
        caption:
          'No separate app to open — you @mention helpbot in the channel, and it answers in a real thread anyone in it can follow.',
        hold: 1300,
        run: (c) => {
          c.postReply(SUPPORT, REQ_TS, AGENT, {
            text: 'Annual plans have a *30-day* refund window from the renewal date. Want me to check a specific account?',
          })
        },
      },
      {
        id: 'home',
        caption:
          'Every agent has an App Home — the at-a-glance hub: what it is, whose account it acts as, and which services it’s connected to.',
        hold: 1600,
        run: (c) => {
          c.appHome(APP)
        },
      },
      {
        id: 'back',
        caption:
          'The thread is the session. It persists across turns, and approvals and status render right here, next to the work — on whichever channel you use.',
        hold: 1400,
        run: (c) => {
          c.switchChannel(SUPPORT)
          c.openThread(REQ_TS)
          c.postReply(SUPPORT, REQ_TS, DEV, { text: 'Perfect, thanks!' })
        },
      },
    ],
  }
}
