// Clip capture: play a Story's beats in a real browser, then render captioned,
// voiced webm/mp4 clips and a poster.
//
// The Story's beats + captions ARE the script: each beat sets its caption on the
// in-page overlay, optionally animates the cursor to click a target, mutates the
// sim, then holds long enough for the narration to finish.
//
// Usage: node engine/capture/clip.mjs <scenario> <outBasePath> [theme] [WxH]
import { chromium } from 'playwright'
import { mkdirSync } from 'node:fs'
import path from 'node:path'
import { narrate } from '../tts/narrate.mjs'
import { encodeClip, remainingSceneHoldMs } from '../tts/mux.mjs'

const scenario = process.argv[2] || 'multiplayer-approval'
const outBase = process.argv[3] || `out/clips/${scenario}`
const theme = process.argv[4] || 'dark'
const [W, H] = (process.argv[5] || '1600x900').split('x').map(Number)
const PORT = process.env.SIM_PORT || process.env.SLACKSIM_PORT || '5178'
const url = `http://127.0.0.1:${PORT}/?scenario=${scenario}&theme=${theme}`

const outDir = path.dirname(outBase)
mkdirSync(outDir, { recursive: true })
// Raw Playwright recordings go to the always-gitignored out/ scratch, never
// next to published media (which may live under site/public, copied there by
// hand).
const rawDir = path.join('out', '_raw')
mkdirSync(rawDir, { recursive: true })

const easeInOutCubic = (t) => (t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2)
const browser = await chromium.launch()
// Synthesize before the recorded context starts: an API call can take seconds,
// and that wait must never become dead air at the front of the finished clip.
const probeContext = await browser.newContext({ viewport: { width: W, height: H } })
const probePage = await probeContext.newPage()
await probePage.goto(url, { waitUntil: 'networkidle' })
await probePage.waitForFunction(() => !!window.__showcaseStory && !!window.__showcase)
const meta = await probePage.evaluate(() => ({
  count: window.__showcaseStory.count,
  intro: window.__showcaseStory.intro,
  captions: window.__showcaseStory.captions,
  holds: window.__showcaseStory.holds,
  clicks: window.__showcaseStory.clicks,
  ids: window.__showcaseStory.ids,
}))
await probeContext.close()
const introText = meta.intro || 'A demo of an OAP agent in Slack.'
const voice = await narrate([
  { id: 'intro', text: introText },
  ...meta.ids.map((id, i) => ({ id, text: meta.captions[i] })),
], path.join('out', 'audio', scenario))
const scenes = []

const context = await browser.newContext({
  viewport: { width: W, height: H },
  deviceScaleFactor: 1,
  recordVideo: { dir: rawDir, size: { width: W, height: H } },
})
const T0 = Date.now()
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

// Intro title card — the story's own caption, or a generic fallback.
await caption(introText)
scenes.push({ path: voice[0].path, startMs: Date.now() - T0 })
await page.waitForTimeout(Math.max(2800, voice[0].durationMs + 500))

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
  const startMs = Date.now() - T0
  scenes.push({ path: voice[i + 1].path, startMs })
  await page.waitForTimeout(remainingSceneHoldMs({
    caption: cap, audioDurationMs: voice[i + 1].durationMs, holdMs: meta.holds[i] ?? 0,
  }))
}

await caption(null)
await page.waitForTimeout(800)

const video = page.video()
await context.close() // flushes the webm
const rawPath = await video.path()
await browser.close()

encodeClip(rawPath, outBase, scenes)
console.log(`wrote:\n  ${outBase}.webm\n  ${outBase}.mp4\n  ${outBase}.png\n(raw: ${rawPath}; voice: ${voice[0].provider})`)
