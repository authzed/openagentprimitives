// Minimal screenshot helper — the seed of the capture engine's still path.
// Usage: node engine/capture/shot.mjs <url> <outPath> [WxH]
import { chromium } from 'playwright'

const url = process.argv[2]
const out = process.argv[3]
const [w, h] = (process.argv[4] || '1600x1000').split('x').map(Number)

const driverExpr = process.argv[5] // optional JS to run against window.__showcase before shooting

const browser = await chromium.launch()
const page = await browser.newPage({ viewport: { width: w, height: h }, deviceScaleFactor: 2 })
await page.goto(url, { waitUntil: 'networkidle' })
await page.waitForTimeout(300)
if (driverExpr) {
  await page.evaluate((expr) => new Function('c', `return (c => { ${expr} })(window.__showcase)`)(window.__showcase), driverExpr)
  await page.waitForTimeout(300)
}
await page.screenshot({ path: out, fullPage: process.env.SHOT_FULLPAGE === '1' })
await browser.close()
console.log(`wrote ${out} (${w}x${h}${process.env.SHOT_FULLPAGE === '1' ? ', fullPage' : ''})`)
