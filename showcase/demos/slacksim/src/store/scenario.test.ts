import { describe, it, expect } from 'vitest'
import { newScenario } from './scenario'
import { SimStore } from './simstore'

describe('scenario builder determinism', () => {
  it('produces byte-identical scenarios across two builds', () => {
    const build = () =>
      newScenario()
        .at('2026-08-28T18:00:00Z')
        .user('U1', 'jordan')
        .bot('U2', 'reviewbot', { badge: 'AGENT' })
        .me('U1')
        .channel('C1', 'project-review')
        .message('C1', 'U2', { text: 'first' })
        .message('C1', 'U1', { text: 'second' })
        .build()
    expect(build()).toEqual(build())
  })

  it('assigns deterministic, strictly increasing timestamps', () => {
    const s = newScenario()
      .at('2026-08-28T18:00:00Z')
      .user('U1', 'a')
      .channel('C1', 'c')
      .message('C1', 'U1', { text: 'one' })
      .message('C1', 'U1', { text: 'two' })
      .message('C1', 'U1', { text: 'three' })
      .build()
    const tss = s.messages.map((m) => Number(m.ts))
    expect(tss[0]).toBeLessThan(tss[1])
    expect(tss[1]).toBeLessThan(tss[2])
    // Fixed epoch from the ISO instant, advancing by a constant 37s step.
    expect(s.messages[0].ts).toMatch(/^\d+\.000000$/)
    expect(tss[1] - tss[0]).toBe(37)
    expect(tss[2] - tss[1]).toBe(37)
  })
})

describe('thread rollups derived, not stored', () => {
  it('computes replyCount, unique replyUserIds, and lastReplyTs on the parent', () => {
    const s = newScenario()
      .at('2026-08-28T18:00:00Z')
      .user('U1', 'jordan')
      .bot('U2', 'reviewbot')
      .channel('C1', 'c')
      .message('C1', 'U2', { text: 'parent', ts: '100.000000' })
      .reply('C1', '100.000000', 'U2', { text: 'r1', ts: '101.000000' })
      .reply('C1', '100.000000', 'U1', { text: 'r2', ts: '102.000000' })
      .reply('C1', '100.000000', 'U2', { text: 'r3', ts: '103.000000' })
      .build()
    const parent = s.messages.find((m) => m.ts === '100.000000')!
    expect(parent.replyCount).toBe(3)
    expect(parent.replyUserIds).toEqual(['U2', 'U1']) // unique, first-seen order
    expect(parent.lastReplyTs).toBe('103.000000')
  })

  it('store.postReply updates the parent rollup live', () => {
    const s = newScenario()
      .at('2026-08-28T18:00:00Z')
      .bot('U2', 'reviewbot')
      .channel('C1', 'c')
      .message('C1', 'U2', { text: 'parent', ts: '100.000000' })
      .build()
    const store = new SimStore(s)
    expect(store.getSnapshot().messages.find((m) => m.ts === '100.000000')!.replyCount).toBeUndefined()
    store.postReply('C1', '100.000000', 'U2', { text: 'live reply' })
    const parent = store.getSnapshot().messages.find((m) => m.ts === '100.000000')!
    expect(parent.replyCount).toBe(1)
    expect(parent.lastReplyTs).toBeDefined()
  })
})
