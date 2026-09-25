// Live still capture of the OAP Desktop Settings editor — the loopback,
// token-gated window the menubar "Settings…" item opens
// (cmd/oap/internal/desktop/settingsui). Unlike webshot.mjs, which logs into
// the password-gated web server on :17080, this hits the per-launch token URL
// the desktop settings window was opened with:
//
//     http://127.0.0.1:<port>/auth?token=<hex>
//
// Get that URL from the live window's argv while Settings is open:
//     ps aux | grep desktop-window | grep -o 'http://127.0.0.1:[0-9]*/auth?token=[a-f0-9]*'
//
// The /auth GET validates the one-shot token, sets the oap_settings_session
// cookie, and redirects to /. A top-level navigation sends no Origin header, so
// desktop.LoopbackGuard passes it; every subsequent /api/* fetch is same-origin
// (127.0.0.1) and loopback too. Read-only: this only navigates and screenshots,
// per showcase/AGENTS.md — it never submits a settings change.
//
// Usage: node engine/capture/settingsshot.mjs <authUrl> <outPath> [tab] [WxH]
//   tab defaults to "Cluster"; WxH defaults to the window's own 760x640.
import { chromium } from 'playwright'

const authUrl = process.argv[2]
const out = process.argv[3] || '/tmp/oap-settings.png'
const tab = process.argv[4] || 'Cluster'
const [w, h] = (process.argv[5] || '760x640').split('x').map(Number)

if (!authUrl) {
  console.error('usage: settingsshot.mjs <authUrl> <outPath> [tab] [WxH]')
  process.exit(1)
}

const browser = await chromium.launch()
// deviceScaleFactor 2 = retina, matching the other real-product stills.
const ctx = await browser.newContext({ viewport: { width: w, height: h }, deviceScaleFactor: 2 })
const page = await ctx.newPage()

await page.goto(authUrl, { waitUntil: 'networkidle' })

// Open the requested top-level tab, then let its data settle. A real click
// generates a real mousedown, which is what Radix Tabs activate on.
const trigger = page.getByRole('tab', { name: tab })
await trigger.waitFor()
await trigger.click()
await page.waitForLoadState('networkidle')
await page.waitForTimeout(600)

await page.screenshot({ path: out })
await browser.close()
console.log(`wrote ${out} (${w}x${h}, retina, tab=${tab})`)
