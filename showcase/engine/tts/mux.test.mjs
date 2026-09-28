import { describe, expect, it } from 'vitest'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { encodeClip, remainingSceneHoldMs } from './mux.mjs'

const run = (bin, args) => {
  const result = spawnSync(bin, args, { encoding: 'utf8' })
  if (result.status !== 0) throw new Error(`${bin}: ${result.stderr}`)
  return result.stdout
}

describe('narrated video output', () => {
  it('holds a scene long enough for speech after its action has run', () => {
    expect(remainingSceneHoldMs({ caption: '', audioDurationMs: 2400, elapsedMs: 700, holdMs: 1000 })).toBe(2200)
  })

  it('keeps a delayed narration track in both published video formats', () => {
    const dir = mkdtempSync(path.join(tmpdir(), 'oap-voice-'))
    const video = path.join(dir, 'source.webm')
    const voice = path.join(dir, 'voice.mp3')
    run('ffmpeg', ['-y', '-loglevel', 'error', '-f', 'lavfi', '-i', 'color=c=black:s=320x180:r=10:d=2', '-c:v', 'libvpx-vp9', video])
    run('ffmpeg', ['-y', '-loglevel', 'error', '-f', 'lavfi', '-i', 'sine=frequency=440:duration=0.4', '-q:a', '5', voice])

    const base = path.join(dir, 'clip')
    encodeClip(video, base, [{ path: voice, startMs: 700 }])
    for (const file of [`${base}.webm`, `${base}.mp4`]) {
      const probe = JSON.parse(run('ffprobe', ['-v', 'error', '-show_streams', '-show_format', '-of', 'json', file]))
      expect(probe.streams.map((stream) => stream.codec_type)).toContain('audio')
      expect(Number(probe.format.duration)).toBeGreaterThan(1.9)
      expect(Number(probe.format.duration)).toBeLessThan(2.2)
    }
  })
})
