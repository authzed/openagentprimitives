// Clean still capture: load a slacksim scenario, play its beats 0..N (applying
// their state mutations) WITHOUT setting the caption overlay, then screenshot.
// This yields a caption-free frame of a specific beat's state, for use as a
// <Screenshot> in the docs (the clip already carries the captions).
//
// Usage: node engine/capture/still.mjs <scenario> <beatIndex> <outPath> [theme] [WxH]
import { chromium } from 'playwright'

const scenario = process.argv[2]
const beatIndex = Number(process.argv[3] ?? 0)
const out = process.argv[4]
const theme = process.argv[5] || 'dark'
const [W, H] = (process.argv[6] || '1600x900').split('x').map(Number)
const PORT = process.env.SIM_PORT || process.env.SLACKSIM_PORT || '5178'

const browser = await chromium.launch()
const page = await browser.newPage({ viewport: { width: W, height: H }, deviceScaleFactor: 2 })
await page.goto(`http://127.0.0.1:${PORT}/?scenario=${scenario}&theme=${theme}`, { waitUntil: 'networkidle' })
await page.waitForFunction(() => !!window.__showcaseStory && !!window.__showcase)
const count = await page.evaluate(() => window.__showcaseStory.count)
for (let i = 0; i <= Math.min(beatIndex, count - 1); i++) {
  await page.evaluate((k) => window.__showcaseStory.run(k), i)
  await page.waitForTimeout(350)
}
await page.evaluate(() => window.__showcase.caption(null)) // ensure no caption overlay
await page.waitForTimeout(300)
await page.screenshot({ path: out })
await browser.close()
console.log(`wrote ${out} (${scenario} @ beat ${beatIndex})`)
