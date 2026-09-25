// Live product capture against a running OAP web server (e.g. the desktop app on
// :17080). Logs in with the local admin password, then captures the real web
// surfaces — sign-in, the New-session modal, a chat exchange, and the admin
// dashboard — for use as onboarding-doc media.
//
// It drives the ISOLATED `pirate-private` demo agent (default ns) so nothing it
// does touches other running sessions, and it does not delete anything it did not
// create. The caller is responsible for cleaning up the session it starts
// (`kubectl delete agentsession -n default pirate-private-<id>`), since the shots
// are the only durable output.
//
// Usage:
//   OAP_ADMIN_PASSWORD=… node engine/capture/webshot.mjs <outPrefix> [message]
//   BASE=http://localhost:17080  (override the target)
import { chromium } from 'playwright'

const BASE = process.env.BASE || 'http://localhost:17080'
const PASS = process.env.OAP_ADMIN_PASSWORD
const OUT = process.argv[2] || '/tmp/oap'
const MSG = process.argv[3] || 'Ahoy! Tell me where the buried treasure lies.'

if (!PASS) { console.error('set OAP_ADMIN_PASSWORD'); process.exit(2) }

const browser = await chromium.launch()
const ctx = await browser.newContext({ viewport: { width: 1440, height: 960 }, deviceScaleFactor: 2 })
const page = await ctx.newPage()
const dump = async (tag) => { await page.screenshot({ path: `${OUT}-${tag}.png` }); console.log('  shot', tag, '::', page.url()) }
const login = async () => {
  const pw = page.locator('input[type=password]')
  if (await pw.count()) {
    await pw.first().fill(PASS)
    await page.locator('button:has-text("Sign in")').first().click()
    await page.waitForTimeout(1500)
  }
}

// Sign-in page (before auth), then log in.
await page.goto(`${BASE}/sessions`, { waitUntil: 'networkidle' })
await page.waitForTimeout(500)
await dump('signin')
await login()

// New-session modal for the pirate demo (requires a Message to enable Start).
await page.locator('button:has-text("New session")').first().click()
await page.waitForTimeout(800)
await page.locator('select').first().selectOption('pirate-private')
await page.locator('textarea').first().fill(MSG)
await page.waitForTimeout(300)
await dump('newsession')

// Start it and let the reply stream in.
await page.locator('button:has-text("Start session")').first().click()
await page.waitForTimeout(4000)
await page.waitForTimeout(14000)
await dump('chat')

// Admin dashboard (re-login if the jump re-challenges).
await page.goto(`${BASE}/admin`, { waitUntil: 'networkidle' }).catch(() => {})
await page.waitForTimeout(1000)
await login()
await dump('admin')

await browser.close()
console.log('done', OUT)
