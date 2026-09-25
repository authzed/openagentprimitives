import { newScenario } from '../store/scenario'
import type { Story } from '../runtime/story'
import type { Block } from '../blockkit/types'
import identityChoiceDynamic from '../fixtures/blockkit/identity-choice-dynamic.json'

// The per-session identity choice. codebot is an ask/dynamic agent: at the start
// of a session it asks whose identity to use — its own, or the person's. Jordan
// asks it to push to *their* fork, so a recommender suggests "Run as me" and
// Jordan confirms. The prompt is OAP's real identity_choice Block Kit (captured
// by `mage blocks:capture`); the resolved card is a small green summary. The
// owner id is U_OWNER so `<@U_OWNER>` resolves to jordan. Fabricated identifiers.

const OWNER = 'U_OWNER' // jordan / "me"
const AGENT = 'U3' // codebot
const ENG = 'C1'
const REQ_TS = '1756800000.000000'
const PROMPT_TS = '1756800200.000000'

const promptBlocks = identityChoiceDynamic as Block[]

// A small resolved summary shown in place once the choice is made.
const chosenBlocks: Block[] = [
  {
    type: 'container',
    rich_text_title: {
      type: 'rich_text',
      elements: [
        {
          type: 'rich_text_section',
          elements: [
            { type: 'emoji', name: 'large_green_circle' },
            { type: 'text', text: '  ' },
            { type: 'text', text: 'Which identity should codebot use for this session?', style: { bold: true } },
          ],
        },
      ],
    },
    child_blocks: [{ type: 'section', text: { type: 'mrkdwn', text: '*Chosen:* Run as <@U_OWNER> (your account)' } }],
    has_header_divider: true,
  },
]

export function identityChoice(): Story {
  const s = newScenario()
    .workspace('Acme Robotics', { glyph: 'A', accent: '#4a154b' })
    .at('2026-08-31T14:00:00Z')
    .user(OWNER, 'jordan')
    .bot(AGENT, 'codebot', { badge: 'AGENT' })
    .me(OWNER)
    .channel('C1', 'eng', { starred: true })
    .channel('C2', 'general')
    .dm('D1', [AGENT])
    .app('A1', 'codebot', { user: AGENT })

  s.message(ENG, OWNER, { text: '<@U3> push the docs fix to *my* fork and open a PR.', ts: REQ_TS })
  s.reply(ENG, REQ_TS, AGENT, {
    text: 'Before I touch anything — should I act as myself, or as you? Pushing to your fork needs your GitHub identity.',
    ts: '1756800100.000000',
  })

  const scenario = s.open(ENG).openThread(REQ_TS).build()

  return {
    scenario,
    intro: 'Agent identity: at session start, codebot asks whose credentials to use — its own, or yours.',
    beats: [
      {
        id: 'ask',
        caption: 'Because this needs your GitHub account, a recommender suggests "Run as me" — but you still confirm.',
        hold: 1100,
        run: (c) => {
          c.postReply(ENG, REQ_TS, AGENT, { blocks: promptBlocks, ts: PROMPT_TS })
          c.setStatus(REQ_TS, 'waiting for you to choose an identity')
        },
      },
      {
        id: 'choose',
        caption: 'Jordan chooses "Run as me" — codebot will act with Jordan’s linked accounts for this session.',
        hold: 900,
        clickAction: 'userPassthrough',
        run: (c) => {
          c.editMessage(ENG, PROMPT_TS, { blocks: chosenBlocks })
          c.setStatus(REQ_TS, undefined)
          c.postReply(ENG, REQ_TS, AGENT, { text: 'Running as *you* — using your linked GitHub account. Cloning your fork now.' })
        },
      },
      {
        id: 'proceed',
        caption: 'The work happens under your identity — the PR is opened as you, not as the agent.',
        hold: 1000,
        run: (c) => {
          c.postReply(ENG, REQ_TS, AGENT, {
            text: 'Pushed to `jordan/agentprimitives` and opened <https://github.com/jordan/agentprimitives/pull/12|PR #12> — all under your GitHub account.',
          })
        },
      },
    ],
  }
}
