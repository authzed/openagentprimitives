import { newScenario } from '../store/scenario'
import type { Story } from '../runtime/story'

// Agent definition: an agent is a reviewable CLASS, and each conversation is a
// disposable SESSION of it — its own scope, budget, and audit trail. Two people
// start two threads with the same agent; each is an independent session, bounded
// by the one class you reviewed. All identifiers are fabricated.

const ME = 'U_OWNER'
const SAM = 'U6'
const RILEY = 'U8'
const AGENT = 'U7' // researchbot
const TEAM = 'C1'
const T1 = '1756700000.000000' // Sam's thread
const T2 = '1756700500.000000' // Riley's thread

export function agentDefinition(): Story {
  const s = newScenario()
    .workspace('Acme Robotics', { glyph: 'A', accent: '#4a154b' })
    .at('2026-08-30T13:00:00Z')
    .user(ME, 'jordan')
    .user(SAM, 'sam')
    .user(RILEY, 'riley')
    .bot(AGENT, 'researchbot', { badge: 'AGENT' })
    .me(ME)
    .channel(TEAM, 'research', { starred: true })

  // Two separate top-level asks to the same agent → two separate sessions.
  s.message(TEAM, SAM, { text: '<@U7> summarize this week’s competitor releases', ts: T1 })
  s.message(TEAM, RILEY, { text: '<@U7> draft a migration plan for the billing service', ts: T2 })

  const scenario = s.open(TEAM).openThread(T1).build()

  return {
    scenario,
    intro: 'Agent definition: one reviewable class, many disposable sessions — each independently scoped.',
    beats: [
      {
        id: 'first',
        caption:
          'An agent is a reviewable *class* — its model, tools, identity, and limits, written down once. This thread is one *session* of that class.',
        hold: 1500,
        run: (c) => {
          c.openThread(T1)
          c.setStatus(T1, 'thinking…')
          c.postReply(TEAM, T1, AGENT, {
            text: 'Here’s the week in competitor releases — three notable launches, summarized with sources. Want the deck?',
          })
          c.setStatus(T1, undefined)
        },
      },
      {
        id: 'second',
        caption:
          'Riley starts a different thread with the *same* class — a second, independent session, with its own scope, its own budget, and its own audit trail.',
        hold: 1600,
        run: (c) => {
          c.openThread(T2)
          c.setStatus(T2, 'thinking…')
          c.postReply(TEAM, T2, AGENT, {
            text: 'Drafted a phased migration plan for the billing service — three phases, each with a rollback. I’ll propose it for approval before touching anything.',
          })
          c.setStatus(T2, undefined)
        },
      },
      {
        id: 'bounded',
        caption:
          'Neither session can exceed what the class allows. The class is the thing you review and trust; a session is the thing that acts — and it’s torn down when it’s done.',
        hold: 1500,
        run: (c) => {
          c.openThread(T2)
        },
      },
    ],
  }
}
