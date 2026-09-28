import { spawnSync } from 'node:child_process'

const run = (bin, args, label) => {
  const result = spawnSync(bin, args, { encoding: 'utf8' })
  if (result.status !== 0) throw new Error(`${label}: ${result.stderr || result.error?.message || result.status}`)
  return result.stdout
}

export function remainingSceneHoldMs({ caption, audioDurationMs, elapsedMs = 0, holdMs = 0 }) {
  const readingMs = Math.min(6500, 1500 + (caption?.length || 0) * 42)
  return Math.max(0, Math.max(holdMs, readingMs, audioDurationMs ? audioDurationMs + 500 : 0) - elapsedMs)
}

// Scenes are { path, startMs } on the *finished* video timeline. The caller
// chooses those starts after its page has reached the state being described.
export function encodeClip(videoPath, outBase, scenes = []) {
  const voiced = scenes.filter((scene) => scene.path)
  const duration = Number(run('ffprobe', ['-v', 'error', '-show_entries', 'format=duration', '-of', 'default=nk=1:nw=1', videoPath], 'video duration').trim())
  if (!Number.isFinite(duration) || duration <= 0) throw new Error(`invalid video duration: ${videoPath}`)

  const inputs = ['-i', videoPath, ...voiced.flatMap((scene) => ['-i', scene.path])]
  const audioFilters = voiced.map((scene, i) =>
    `[${i + 1}:a]adelay=${Math.max(0, Math.round(scene.startMs))}:all=1[a${i}]`)
  const mixed = voiced.length === 1
    ? `[a0]apad,atrim=duration=${duration}[voice]`
    : `[${voiced.map((_, i) => `a${i}`).join('][')}]amix=inputs=${voiced.length}:duration=longest:dropout_transition=0,apad,atrim=duration=${duration}[voice]`
  const audio = voiced.length ? ['-filter_complex', [...audioFilters, mixed].join(';'), '-map', '[voice]'] : ['-an']

  for (const [ext, videoCodec, audioCodec, extra] of [
    ['webm', ['-c:v', 'libvpx-vp9', '-b:v', '0', '-crf', '32'], ['-c:a', 'libopus'], []],
    ['mp4', ['-c:v', 'libx264', '-pix_fmt', 'yuv420p', '-crf', '20'], ['-c:a', 'aac', '-b:a', '160k'], ['-movflags', '+faststart']],
  ]) {
    run('ffmpeg', ['-y', '-loglevel', 'error', ...inputs, ...audio, '-map', '0:v:0', ...videoCodec,
      ...(voiced.length ? audioCodec : []), ...extra, '-t', String(duration), `${outBase}.${ext}`], `${ext} render`)
  }
  run('ffmpeg', ['-y', '-loglevel', 'error', '-ss', String(Math.min(3, duration / 2)), '-i', videoPath,
    '-frames:v', '1', `${outBase}.png`], 'poster')
}
