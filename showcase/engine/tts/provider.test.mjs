import { describe, it, expect } from 'vitest'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { mkdtempSync } from 'node:fs'
import { selectProvider, elevenLabsProvider, openAIProvider } from './provider.mjs'
import { narrate } from './narrate.mjs'

describe('TTS provider selection', () => {
  it('prefers ElevenLabs when its key is present', () => {
    const p = selectProvider({ ELEVENLABS_API_KEY: 'x', OPENAI_API_KEY: 'y' })
    expect(p?.name).toBe('elevenlabs')
  })

  it('falls back to OpenAI when only its key is present', () => {
    const p = selectProvider({ OPENAI_API_KEY: 'y' })
    expect(p?.name).toBe('openai')
  })

  it('returns null (caption-only) when no key is present', () => {
    expect(selectProvider({})).toBeNull()
  })
})

describe('TTS cache keys', () => {
  it('are stable for the same text/voice/model', () => {
    const p = elevenLabsProvider('k', {})
    expect(p.cacheKey('hello world')).toBe(p.cacheKey('hello world'))
  })

  it('change when the voice changes, so a new voice re-synthesizes', () => {
    const a = elevenLabsProvider('k', { TTS_VOICE: 'voiceA' })
    const b = elevenLabsProvider('k', { TTS_VOICE: 'voiceB' })
    expect(a.cacheKey('same text')).not.toBe(b.cacheKey('same text'))
  })

  it('differ between backends for identical text', () => {
    const el = elevenLabsProvider('k', {})
    const oa = openAIProvider('k', {})
    expect(el.cacheKey('hi')).not.toBe(oa.cacheKey('hi'))
  })
})

describe('narrate with no provider (the default, key-free path)', () => {
  it('returns null audio paths and zero durations without calling out', async () => {
    const dir = mkdtempSync(path.join(tmpdir(), 'tts-'))
    const results = await narrate([{ id: 'intro', text: 'hello' }, { id: 'beat1', text: 'world' }], dir, {})
    expect(results).toHaveLength(2)
    for (const r of results) {
      expect(r.path).toBeNull()
      expect(r.durationMs).toBe(0)
      expect(r.provider).toBe('none')
    }
  })
})
