// Clip capture: play a Story's beats in a real browser, recording a narrated
// (caption-driven) webm, then transcode to mp4 + extract a poster with ffmpeg.
//
// The Story's beats + captions ARE the script: each beat sets its caption on the
// in-page overlay, optionally animates the cursor to click a target, mutates the
// sim, then holds for a reading-time-derived dwell. A pluggable TTS layer can
// later replace captions with narration audio; the timing model is the same.
//
// Usage: node engine/capture/clip.mjs <scenario> <outBasePath> [theme] [WxH]
import { chromium } from 'playwright'
import { spawnSync } from 'node:child_process'
import { mkdirSync } from 'node:fs'
import path from 'node:path'

const scenario = process.argv[2] || 'multiplayer-approval'
const outBase = process.argv[3] || `out/clips/${scenario}`
const theme = process.argv[4] || 'dark'
const [W, H] = (process.argv[5] || '1600x900').split('x').map(Number)
const PORT = process.env.SIM_PORT || process.env.SLACKSIM_PORT || '5178'
const url = `http://localhost:${PORT}/?scenario=${scenario}&theme=${theme}`

const outDir = path.dirname(outBase)
mkdirSync(outDir, { recursive: true })
// Raw Playwright recordings go to the always-gitignored out/ scratch, never
// next to published media (which may live under site/public, copied there by
// hand).
const rawDir = path.join('out', '_raw')
mkdirSync(rawDir, { recursive: true })

const easeInOutCubic = (t) => (t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2)
const readingMs = (text) => Math.min(6500, 1500 + (text ? text.length : 0) * 42)

const browser = await chromium.launch()
const context = await browser.newContext({
  viewport: { width: W, height: H },
  deviceScaleFactor: 1,
  recordVideo: { dir: rawDir, size: { width: W, height: H } },
})
const page = await context.newPage()
await page.goto(url, { waitUntil: 'networkidle' })
await page.waitForFunction(() => !!window.__showcaseStory && !!window.__showcase)
await page.waitForTimeout(600)

const caption = (t) => page.evaluate((x) => window.__showcase.caption(x), t)
const pointer = (x, y, down) => page.evaluate(([a, b, d]) => window.__showcase.pointer(a, b, d), [x, y, down])
const runBeat = (i) => page.evaluate((k) => window.__showcaseStory.run(k), i)
const rectOf = (sel) =>
  page.evaluate((s) => {
    const el = document.querySelector(s)
    if (!el) return null
    const r = el.getBoundingClientRect()
    return { x: r.x, y: r.y, w: r.width, h: r.height }
  }, sel)

let curX = W * 0.5
let curY = H * 0.62
async function moveTo(x, y, ms) {
  const steps = Math.max(2, Math.round(ms / 16))
  const sx = curX
  const sy = curY
  for (let i = 1; i <= steps; i++) {
    const p = easeInOutCubic(i / steps)
    await pointer(sx + (x - sx) * p, sy + (y - sy) * p, false)
    await page.waitForTimeout(16)
  }
  curX = x
  curY = y
}
async function clickHere() {
  await pointer(curX, curY, true)
  await page.waitForTimeout(240)
  await pointer(curX, curY, false)
  await page.waitForTimeout(120)
}

const meta = await page.evaluate(() => ({
  count: window.__showcaseStory.count,
  intro: window.__showcaseStory.intro,
  captions: window.__showcaseStory.captions,
  holds: window.__showcaseStory.holds,
  clicks: window.__showcaseStory.clicks,
  ids: window.__showcaseStory.ids,
}))

// Intro title card — the story's own caption, or a generic fallback.
await caption(meta.intro || 'A demo of an OAP agent in Slack.')
await page.waitForTimeout(2800)

for (let i = 0; i < meta.count; i++) {
  const cap = meta.captions[i]
  await caption(cap ?? null)
  await page.waitForTimeout(500)

  // Animate a real cursor click on a button before the beat runs: the beat's
  // explicit clickAction, else the "approve" button on an approval beat. Only one
  // matching button is visible at a time (resolved cards drop it; switching
  // surfaces hides others).
  const clickAction =
    meta.clicks[i] || (meta.ids[i] === 'approve' || meta.ids[i].endsWith('-approve') ? 'approve' : '')
  if (clickAction) {
    const r = await rectOf(`.sk-btn[data-action-id="${clickAction}"]`)
    if (r) {
      await moveTo(r.x + r.w / 2, r.y + r.h / 2, 750)
      await page.waitForTimeout(180)
      await clickHere()
    }
  }

  await runBeat(i)
  await page.waitForTimeout(Math.max(meta.holds[i] ?? 0, readingMs(cap)))
}

await caption(null)
await page.waitForTimeout(800)

const video = page.video()
await context.close() // flushes the webm
const rawPath = await video.path()
await browser.close()

// ffmpeg post: a clean webm (VP9), an mp4 (H.264) for broad compat, and a poster.
const webmOut = `${outBase}.webm`
const mp4Out = `${outBase}.mp4`
const posterOut = `${outBase}.png`

function ff(args, label) {
  const r = spawnSync('ffmpeg', ['-y', '-loglevel', 'error', ...args], { stdio: 'inherit' })
  if (r.status !== 0) throw new Error(`ffmpeg ${label} failed (${r.status})`)
}
ff(['-i', rawPath, '-c:v', 'libvpx-vp9', '-b:v', '0', '-crf', '32', '-an', webmOut], 'webm')
ff(['-i', rawPath, '-c:v', 'libx264', '-pix_fmt', 'yuv420p', '-crf', '20', '-movflags', '+faststart', '-an', mp4Out], 'mp4')
ff(['-ss', '3', '-i', rawPath, '-frames:v', '1', posterOut], 'poster')

console.log(`wrote:\n  ${webmOut}\n  ${mp4Out}\n  ${posterOut}\n(raw: ${rawPath})`)
