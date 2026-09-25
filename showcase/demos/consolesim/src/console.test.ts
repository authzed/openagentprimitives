import { describe, it, expect } from 'vitest'
import { bold, green, ok } from './term/ansi'
import { bindStory, type TermControl } from './runtime/story'

describe('ansi', () => {
  it('wraps text in an SGR pair and resets', () => {
    expect(green('x')).toBe('\x1b[32mx\x1b[0m')
    expect(bold('y')).toBe('\x1b[1my\x1b[0m')
  })
  it('glyphs carry their color', () => {
    expect(ok).toContain('✓')
    expect(ok).toContain('\x1b[32m') // green
  })
})

describe('bindStory', () => {
  it('exposes the runtime and drives a beat through the async control', async () => {
    const calls: string[] = []
    const control: TermControl = {
      prompt: async () => void calls.push('prompt'),
      type: async (t) => void calls.push('type:' + t),
      line: async () => void calls.push('line'),
      write: async () => {},
      enter: async () => {},
      wait: async () => {},
      clear: async () => {},
      caption: () => {},
    }
    bindStory(
      {
        intro: 'x',
        beats: [
          {
            id: 'a',
            caption: 'hi',
            run: async (c) => {
              await c.prompt()
              await c.type('oap init')
            },
          },
        ],
      },
      control,
    )
    const rt = window.__showcaseStory!
    expect(rt.count).toBe(1)
    expect(rt.captions).toEqual(['hi'])
    expect(rt.ids).toEqual(['a'])
    // run returns the beat's promise, so awaiting it spans the full animation.
    await rt.run(0)
    expect(calls).toEqual(['prompt', 'type:oap init'])
  })
})
