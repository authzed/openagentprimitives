import { newScenario } from '../store/scenario'
import type { Story } from '../runtime/story'
import type { Block } from '../blockkit/types'
import approval from '../fixtures/blockkit/approval.json'
import approvalResolved from '../fixtures/blockkit/approval-resolved.json'
import joinApproval from '../fixtures/blockkit/join-approval.json'
import joinApprovalResolved from '../fixtures/blockkit/join-approval-resolved.json'
import plan from '../fixtures/blockkit/plan.json'
import message from '../fixtures/blockkit/message.json'

// A multiplayer Slack session with TWO owner gates. First, when a second human
// (alex) asks to join, an owner (jordan) must approve before alex's message is
// acted on. Then the agent's own sensitive action — the production deploy —
// needs owner approval too.
//
// Every agent card is the REAL Block Kit OAP emits, captured by
// `mage blocks:capture`: the plan checklist, the join-participation approval
// (yellow tone) and its resolved twin, the deploy approval (blue tone) and its
// resolved twin, and the completion message. Only the human framing is authored
// here. The owner's user id is U_OWNER so the fixtures' `<@U_OWNER>` decider
// mention resolves to jordan, and alex is U4 so `<@U4>` resolves to alex. All
// identifiers are fabricated.

const OWNER = 'U_OWNER' // jordan, the thread owner / "me"
const AGENT = 'U2' // srebot
const ALEX = 'U4'
const DEPLOY = 'C1'
const DM_JORDAN = 'D1'
const REQ_TS = '1756500000.000000' // jordan's request — the thread parent
const JOIN_CARD_TS = '1756500400.000000'
const DEPLOY_THREAD_TS = '1756500700.000000'
const DEPLOY_DM_TS = '1756500800.000000'

const approvalBlocks = approval as Block[]
const approvalResolvedBlocks = approvalResolved as Block[]
const joinBlocks = joinApproval as Block[]
const joinResolvedBlocks = joinApprovalResolved as Block[]
const planBlocks = plan as Block[]
const completeBlocks = message as Block[]

// The deploy DM wraps the real card with a one-line reason the owner is pinged.
function dmLead(blocks: Block[]): Block[] {
  return [
    {
      type: 'section',
      text: {
        type: 'mrkdwn',
        text: "You're a thread owner of a session in <#C1|deploys>. *srebot* needs your approval before it can continue:",
      },
    },
    { type: 'divider' },
    ...blocks,
  ]
}

export function multiplayerApproval(): Story {
  const s = newScenario()
    .workspace('Acme Robotics', { glyph: 'A', accent: '#4a154b' })
    .at('2026-08-28T18:24:00Z')
    .user(OWNER, 'jordan')
    .user(ALEX, 'alex')
    .user('U5', 'morgan')
    .bot(AGENT, 'srebot', { badge: 'AGENT' })
    .me(OWNER)
    .channel('C1', 'deploys', { starred: true })
    .channel('C6', 'incidents')
    .channel('C3', 'general')
    .dm('D1', [AGENT]) // jordan ↔ srebot (morgan's owner DM is off-screen)
    .app('A1', 'srebot', { user: AGENT })

  // Initial thread: jordan asks srebot to roll out; srebot acknowledges + plans.
  s.message(DEPLOY, OWNER, { text: '<@U2> please roll out `hotfix-1.4.2` to production when you get a sec.', ts: REQ_TS })
  s.reply(DEPLOY, REQ_TS, AGENT, { text: 'On it, <@U_OWNER> — planning the rollout of `hotfix-1.4.2` now.', ts: '1756500100.000000' })
  s.reply(DEPLOY, REQ_TS, AGENT, { blocks: planBlocks, ts: '1756500200.000000' })

  const scenario = s.open(DEPLOY).openThread(REQ_TS).build()

  return {
    scenario,
    intro: 'Multiplayer sessions in Slack: two owner gates — who may take part, and which actions run.',
    beats: [
      {
        id: 'alex-joins',
        caption: 'A second teammate, Alex, opens the thread and asks to help drive the rollout.',
        hold: 800,
        run: (c) => {
          c.postReply(DEPLOY, REQ_TS, ALEX, { text: 'Alex joined the thread.', subtype: 'thread_join', ts: '1756500300.000000' })
          c.postReply(DEPLOY, REQ_TS, ALEX, { text: 'mind if I jump in and help drive this one? :eyes:', ts: '1756500350.000000' })
        },
      },
      {
        id: 'join-approval',
        caption: 'A new participant is a decision, so srebot asks an owner to approve Alex before acting on his message.',
        hold: 900,
        run: (c) => {
          c.postReply(DEPLOY, REQ_TS, AGENT, { blocks: joinBlocks, ts: JOIN_CARD_TS })
          c.setStatus(REQ_TS, 'waiting for an owner to approve Alex joining')
        },
      },
      {
        id: 'join-approve',
        caption: 'Jordan approves — Alex is in, and only now is his message picked up.',
        hold: 900,
        run: (c) => {
          c.editMessage(DEPLOY, JOIN_CARD_TS, { blocks: joinResolvedBlocks })
          c.setStatus(REQ_TS, undefined)
          c.postReply(DEPLOY, REQ_TS, AGENT, { text: 'Approved by <@U_OWNER>. <@U4>, you’re in — I’ll check with you before the risky steps.' })
        },
      },
      {
        id: 'deploy-approval',
        caption: 'srebot reaches the production deploy — a sensitive action that also needs an owner’s sign-off.',
        hold: 900,
        run: (c) => {
          c.postReply(DEPLOY, REQ_TS, AGENT, { blocks: approvalBlocks, ts: DEPLOY_THREAD_TS })
          c.postMessage(DM_JORDAN, AGENT, { blocks: dmLead(approvalBlocks), ts: DEPLOY_DM_TS })
          c.setStatus(REQ_TS, 'waiting for an owner to approve the production deploy')
          c.switchChannel(DM_JORDAN)
        },
      },
      {
        id: 'deploy-approve',
        caption: 'Jordan approves the deploy right from the DM — the real card resolves in place (blue turns green).',
        hold: 900,
        run: (c) => {
          c.editMessage(DM_JORDAN, DEPLOY_DM_TS, { blocks: dmLead(approvalResolvedBlocks) })
          c.postMessage(DM_JORDAN, AGENT, { text: 'Thanks — recording your approval and continuing in <#C1|deploys>.' })
        },
      },
      {
        id: 'deploy-resolve',
        caption: 'The approval propagates back to the thread — everyone watching sees it resolve.',
        hold: 900,
        run: (c) => {
          c.switchChannel(DEPLOY)
          c.openThread(REQ_TS)
          c.editMessage(DEPLOY, DEPLOY_THREAD_TS, { blocks: approvalResolvedBlocks })
          c.postReply(DEPLOY, REQ_TS, AGENT, { text: 'Approval received from <@U_OWNER>. Proceeding with the rollout.' })
          c.setStatus(REQ_TS, 'deploying hotfix-1.4.2 to prod-us-east…')
        },
      },
      {
        id: 'complete',
        caption: 'srebot completes the rollout and reports back to the whole thread.',
        hold: 1000,
        run: (c) => {
          c.setStatus(REQ_TS, undefined)
          c.postReply(DEPLOY, REQ_TS, AGENT, { blocks: completeBlocks })
        },
      },
    ],
  }
}
