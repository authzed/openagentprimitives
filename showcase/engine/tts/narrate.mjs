import { readFileSync, writeFileSync, existsSync, mkdirSync } from 'node:fs'
import { spawnSync } from 'node:child_process'
import path from 'node:path'
import { selectProvider } from './provider.mjs'

// Measure an audio file's duration in ms via ffprobe (0 if unreadable).
export function probeDurationMs(file) {
  const r = spawnSync(
    'ffprobe',
    ['-v', 'error', '-show_entries', 'format=duration', '-of', 'default=nk=1:nw=1', file],
    { encoding: 'utf8' },
  )
  const secs = parseFloat((r.stdout || '').trim())
  return Number.isFinite(secs) ? Math.round(secs * 1000) : 0
}

/**
 * Synthesize narration for scenes [{ id, text }] into audioDir, caching each
 * mp3 on the provider's content hash (a `.hash` sidecar) so editing one line
 * re-synthesizes only that line. Returns [{ id, path, durationMs, provider }].
 *
 * With no TTS key, the provider is null: every path is null, durationMs 0, and
 * the caller narrates via on-screen captions only. This is the default and the
 * only path exercised without credentials.
 */
export async function narrate(scenes, audioDir, env = process.env) {
  const provider = selectProvider(env)
  mkdirSync(audioDir, { recursive: true })
  const results = []
  for (const scene of scenes) {
    const text = (scene.text || '').trim()
    if (!provider || !text) {
      results.push({ id: scene.id, path: null, durationMs: 0, provider: provider?.name ?? 'none' })
      continue
    }
    const mp3 = path.join(audioDir, `${scene.id}.mp3`)
    const hashPath = `${mp3}.hash`
    const key = provider.cacheKey(text)
    const cached = existsSync(mp3) && existsSync(hashPath) && readFileSync(hashPath, 'utf8').trim() === key
    if (!cached) {
      const buf = await provider.synthesize(text)
      writeFileSync(mp3, buf)
      writeFileSync(hashPath, key)
    }
    results.push({ id: scene.id, path: mp3, durationMs: probeDurationMs(mp3), provider: provider.name })
  }
  return results
}
