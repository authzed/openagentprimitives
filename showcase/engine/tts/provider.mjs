import { createHash } from 'node:crypto'

// Pluggable narration TTS. One interface, two backends, selected by which API
// key is present (ElevenLabs preferred — it's what the design settled on). With
// no key, selectProvider returns null and the clip pipeline stays caption-only.
//
// A provider is { name, cacheKey(text), async synthesize(text) -> Buffer(mp3) }.
// cacheKey folds in voice+model so changing the voice re-synthesizes; the caller
// caches on it (content-hash sidecar), exactly like the rest of the pipeline.

const sha = (s) => createHash('sha256').update(s).digest('hex')

export function elevenLabsProvider(apiKey, env = process.env) {
  const voiceId = env.TTS_VOICE || 'nPczCjzI2devNBz1zQrb' // "Brian" — deep, calm
  const model = env.TTS_MODEL || 'eleven_multilingual_v2'
  return {
    name: 'elevenlabs',
    voiceId,
    model,
    cacheKey: (text) => sha(`elevenlabs:${voiceId}:${model}:${text}`),
    async synthesize(text) {
      const res = await fetch(`https://api.elevenlabs.io/v1/text-to-speech/${voiceId}`, {
        method: 'POST',
        headers: { 'xi-api-key': apiKey, 'Content-Type': 'application/json', Accept: 'audio/mpeg' },
        body: JSON.stringify({ text, model_id: model }),
      })
      if (!res.ok) throw new Error(`elevenlabs TTS ${res.status}: ${await res.text().catch(() => '')}`)
      return Buffer.from(await res.arrayBuffer())
    },
  }
}

export function openAIProvider(apiKey, env = process.env) {
  const voice = env.TTS_VOICE || 'alloy'
  const model = env.TTS_MODEL || 'gpt-4o-mini-tts'
  return {
    name: 'openai',
    voiceId: voice,
    model,
    cacheKey: (text) => sha(`openai:${voice}:${model}:${text}`),
    async synthesize(text) {
      const res = await fetch('https://api.openai.com/v1/audio/speech', {
        method: 'POST',
        headers: { Authorization: `Bearer ${apiKey}`, 'Content-Type': 'application/json' },
        body: JSON.stringify({ model, voice, input: text, response_format: 'mp3' }),
      })
      if (!res.ok) throw new Error(`openai TTS ${res.status}: ${await res.text().catch(() => '')}`)
      return Buffer.from(await res.arrayBuffer())
    },
  }
}

/**
 * Choose a TTS provider from the environment. ElevenLabs wins when both keys are
 * set. Returns null when no key is present — the caller then narrates via
 * on-screen captions only.
 */
export function selectProvider(env = process.env) {
  if (env.ELEVENLABS_API_KEY) return elevenLabsProvider(env.ELEVENLABS_API_KEY, env)
  if (env.OPENAI_API_KEY) return openAIProvider(env.OPENAI_API_KEY, env)
  return null
}
