// Live product clip capture: play a Story's beats against a RUNNING OAP web
// server (e.g. the desktop app on :17080), recording the real browser, then cut
// the recording down to each beat's window and assemble a captioned webm + mp4 +
// poster with ffmpeg. The sibling of clip.mjs (which plays a simulator): same
// caption-driven narration, same output shape, but the page is the product.
//
// A Story is an ES module exporting { intro, beats }. Each beat is
//   { id, caption, run: async (ctx) => {}, hold: seconds, shot?: name }
// run() performs the beat's action (a click, a typed message, an API call) and
// returns once the page shows the state the caption describes; the engine then
// holds for `hold` seconds, takes the optional still, and marks the window
// [runStart - lead, holdEnd] as kept. Dead time between beats (a model thinking)
// is cut, so the clip shows only what the person would watch happen.
//
// ctx: { page, base, ns, sess, login(), api(method, path, body), say(text),
//        decide(category, ref, action), pending(), waitFor(fn, {timeoutMs, everyMs}),
//        composed(), steps(), caption(text), pointerTo(locator), click(locator), still(name), log }
//
// Usage:
//   OAP_ADMIN_PASSWORD=… node engine/capture/webclip.mjs <story.mjs> <outBasePath> [WxH]
//   BASE=http://127.0.0.1:17080  NS=agentprimitives-system  CLASS=agent-builder
//   SHOTS_DIR=<dir for stills> (default: dirname(outBasePath))
//   KUBE_CONTEXT=ap-desktop     (pending interactions are read with kubectl)
import { chromium } from 'playwright'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { pathToFileURL } from 'node:url'

const storyPath = process.argv[2]
const outBase = process.argv[3] || 'out/clips/webclip'
const [W, H] = (process.argv[4] || '1600x900').split('x').map(Number)
const BASE = process.env.BASE || 'http://127.0.0.1:17080'
const PASS = process.env.OAP_ADMIN_PASSWORD
const NS = process.env.NS || 'agentprimitives-system'
const CLASS = process.env.CLASS || 'agent-builder'
const KCTX = process.env.KUBE_CONTEXT || 'ap-desktop'
const SHOTS = process.env.SHOTS_DIR || path.dirname(outBase)
if (!storyPath) { console.error('usage: webclip.mjs <story.mjs> <outBasePath> [WxH]'); process.exit(2) }
if (!PASS) { console.error('set OAP_ADMIN_PASSWORD'); process.exit(2) }

const story = await import(pathToFileURL(path.resolve(storyPath)).href)
const rawDir = path.join('out', '_raw')
fs.mkdirSync(rawDir, { recursive: true })
fs.mkdirSync(path.dirname(outBase), { recursive: true })
fs.mkdirSync(SHOTS, { recursive: true })

const log = (...a) => console.log(`[${new Date().toISOString().slice(11, 19)}]`, ...a)
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

// The caption/pointer overlay, injected into every document the context loads
// so it survives the shell's navigations. Same CSS as the simulators' overlay
// (demos/slacksim/src/styles/overlay.css) so clips read alike.
const OVERLAY = `
(() => {
  if (window.__cap) return
  const css = \`
  .sk-overlay{position:fixed;inset:0;pointer-events:none;z-index:99999}
  .sk-ov-cursor{position:absolute;top:0;left:0;width:26px;height:26px;will-change:transform;filter:drop-shadow(0 1px 2px rgba(0,0,0,.5))}
  .sk-ov-cursor .sk-ov-ring{position:absolute;top:2px;left:2px;width:22px;height:22px;border-radius:50%;border:2px solid rgba(29,155,209,.9);transform:scale(0);opacity:0}
  .sk-ov-cursor.is-down .sk-ov-ring{animation:sk-click .4s ease-out}
  @keyframes sk-click{0%{transform:scale(0);opacity:.9}100%{transform:scale(2.2);opacity:0}}
  .sk-ov-caption{position:fixed;left:50%;bottom:48px;transform:translateX(-50%);max-width:min(1100px,78vw);padding:12px 22px;border-radius:12px;background:rgba(11,12,14,.86);backdrop-filter:blur(2px);box-shadow:0 6px 24px rgba(0,0,0,.4);text-align:center}
  .sk-ov-caption-text{color:#fff;font-family:system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;font-size:22px;font-weight:600;line-height:1.4;letter-spacing:.01em}
  \`
  const install = () => {
    if (document.querySelector('.sk-overlay')) return
    const style = document.createElement('style'); style.textContent = css; document.head.appendChild(style)
    const root = document.createElement('div'); root.className = 'sk-overlay'
    root.innerHTML = '<div class="sk-ov-cursor" hidden><svg width="26" height="26" viewBox="0 0 26 26" fill="none" xmlns="http://www.w3.org/2000/svg"><path d="M5 3l14 7-6 1.5L10 20 5 3z" fill="#111" stroke="#fff" stroke-width="1.5" stroke-linejoin="round"/></svg><span class="sk-ov-ring"></span></div><div class="sk-ov-caption" hidden><span class="sk-ov-caption-text"></span></div>'
    document.body.appendChild(root)
  }
  const el = (sel) => document.querySelector(sel)
  window.__cap = {
    caption(t) { install(); const c = el('.sk-ov-caption'); if (!t) { c.hidden = true; return } el('.sk-ov-caption-text').textContent = t; c.hidden = false },
    pointer(x, y, down) { install(); const c = el('.sk-ov-cursor'); c.hidden = false; c.style.transform = 'translate(' + x + 'px,' + y + 'px)'; c.classList.toggle('is-down', !!down) },
    hidePointer() { const c = el('.sk-ov-cursor'); if (c) c.hidden = true },
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', install); else install()
  // Re-set the caption after React mounts (the shell replaces <body> children).
  new MutationObserver(() => { if (!document.querySelector('.sk-overlay')) install() }).observe(document.documentElement, { childList: true, subtree: true })
})()`

const browser = await chromium.launch()
const ctxOpts = {
  viewport: { width: W, height: H },
  deviceScaleFactor: 1,
  recordVideo: { dir: rawDir, size: { width: W, height: H } },
  extraHTTPHeaders: { Origin: BASE },
}
const context = await browser.newContext(ctxOpts)
const T0 = Date.now()
await context.addInitScript(OVERLAY)
const page = await context.newPage()
const kept = [] // [fromMs, toMs] relative to T0

const sess = { ns: NS, name: '' }
const api = async (method, p, body) => {
  const res = await page.request.fetch(`${BASE}${p}`, { method, data: body, headers: { 'content-type': 'application/json' } })
  let json = null
  try { json = await res.json() } catch { json = null }
  return { status: res.status(), json }
}
const login = async () => {
  const pw = page.locator('input[type=password]')
  if (await pw.count()) {
    await pw.first().fill(PASS)
    await page.locator('button:has-text("Sign in")').first().click()
    await page.waitForTimeout(1500)
  }
}
const kubectl = (...args) => {
  const r = spawnSync('kubectl', ['--context', KCTX, ...args], { encoding: 'utf8' })
  if (r.status !== 0) throw new Error(`kubectl ${args.join(' ')}: ${r.stderr}`)
  return r.stdout
}
// pending returns the session's pending interactions as [{category, ref, tool}].
const pending = () => {
  if (!sess.name) return []
  // The session status names a card's category and request, not the tool
  // behind it; which tool raised a card is in the session's plan-gate and
  // authorization records (oap memory list <session>).
  const out = kubectl('-n', sess.ns, 'get', 'agentsession', sess.name, '-o',
    'jsonpath={range .status.pendingInteractions[*]}{.category}|{.requestRef}{"\\n"}{end}')
  return out.split('\n').filter(Boolean).map((l) => { const [category, ref] = l.split('|'); return { category, ref } })
}
const phase = () => sess.name ? kubectl('-n', sess.ns, 'get', 'agentsession', sess.name, '-o', 'jsonpath={.status.phase}').trim() : ''
const say = (text) => api('POST', `/sessions/api/${sess.ns}/${sess.name}/message`, { text })
const decide = (category, ref, action = 'approve') =>
  api('POST', `/sessions/api/${sess.ns}/${sess.name}/decision`, { category, requestRef: ref, actionId: action })
// The page's own DOM is the state probe: the agent-UI renders every hook as
// <section data-hook="…"> and marks the ones the agent has painted with
// data-agent-composed="true"; the stage timeline marks the active step with
// aria-current="step". Reading the DOM rather than a declaration endpoint keeps
// the probe on the thing the viewer sees.
const composed = () => page.$$eval('[data-hook][data-agent-composed="true"]', (els) => els.map((e) => e.getAttribute('data-hook'))).catch(() => [])
const STEP_ORDER = ['assess', 'tools', 'permissions', 'build', 'test', 'deliver']
const STEP_LABELS = ['Intake', 'Tools', 'Permissions', 'Build', 'Test', 'Deliver']
const steps = async () => {
  const text = await page.$eval('[aria-current="step"]', (e) => e.textContent || '').catch(() => '')
  // The rail renders the step number before the label ("1Intake …"), so match the
  // label after any leading digits/space rather than at the very start.
  const head = text.trim().replace(/^[\d\s]+/, '')
  const active = STEP_LABELS.findIndex((l) => head.startsWith(l))
  const out = {}
  STEP_ORDER.forEach((id, i) => { out[id] = active < 0 ? 'unknown' : i < active ? 'done' : i === active ? 'active' : 'upcoming' })
  return out
}
const waitFor = async (fn, { timeoutMs = 300000, everyMs = 3000, what = 'condition' } = {}) => {
  const t = Date.now()
  for (;;) {
    let ok = false
    try { ok = await fn() } catch (e) { log('waitFor probe errored (retrying):', e.message) }
    if (ok) return
    if (Date.now() - t > timeoutMs) throw new Error(`timed out waiting for ${what}`)
    await sleep(everyMs)
  }
}
const caption = (t) => page.evaluate((x) => window.__cap && window.__cap.caption(x), t).catch(() => {})
const easeInOutCubic = (t) => (t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2)
let cur = { x: W / 2, y: H / 2 }
const pointerTo = async (locator, { down = false, ms = 700 } = {}) => {
  const box = await locator.boundingBox().catch(() => null)
  if (!box) return
  const to = { x: box.x + box.width / 2, y: box.y + box.height / 2 }
  const steps = 24
  for (let i = 1; i <= steps; i++) {
    const k = easeInOutCubic(i / steps)
    const x = cur.x + (to.x - cur.x) * k, y = cur.y + (to.y - cur.y) * k
    await page.evaluate(([a, b]) => window.__cap && window.__cap.pointer(a, b, false), [x, y]).catch(() => {})
    await sleep(ms / steps)
  }
  cur = to
  if (down) await page.evaluate(([a, b]) => window.__cap && window.__cap.pointer(a, b, true), [to.x, to.y]).catch(() => {})
}
// The shell pops a "the agent is waiting for your reply" modal when the agent
// ends a turn on a question; it covers the page. A person would press Later and
// keep working on the page, so every click does the same first.
const dismissReplyModal = async () => {
  const modal = page.getByTestId('agent-ui-reply-modal')
  if (!(await modal.isVisible().catch(() => false))) return
  const later = modal.getByRole('button', { name: /Later|Not now|Dismiss|Close/ }).first()
  if (await later.isVisible().catch(() => false)) { log('dismissing the reply modal'); await later.click(); await sleep(500); return }
  await page.keyboard.press('Escape'); await sleep(500)
}
// The modal can pop between the dismissal and the click — the agent parks on a
// question while the pointer is still travelling — so the click is retried in
// short rounds, each behind a fresh dismissal, rather than one long wait under
// an overlay that intercepts it.
const click = async (locator, { rounds = 8 } = {}) => {
  await dismissReplyModal()
  await pointerTo(locator, { down: true })
  for (let i = 1; ; i++) {
    try { await locator.click({ timeout: 5000 }); break } catch (e) {
      if (i >= rounds) throw e
      log(`click round ${i} did not land (${String(e.message).split('\n')[0]}); dismissing any modal and retrying`)
      await dismissReplyModal()
    }
  }
  await sleep(400)
}
// A still is caption-free: the doc page carries its own caption, and the pointer
// is a clip aid. Both are hidden for the shot and the caption restored after.
const still = async (name) => {
  const p = path.join(SHOTS, `${name}.png`)
  // A doc still shows the page, not the reply modal that may have popped over
  // it while the beat held — a person would have pressed Later.
  await dismissReplyModal()
  const current = await page.evaluate(() => { const t = document.querySelector('.sk-ov-caption-text'); const c = document.querySelector('.sk-ov-caption'); return c && !c.hidden ? t.textContent : null }).catch(() => null)
  await page.evaluate(() => { window.__cap && window.__cap.hidePointer(); window.__cap && window.__cap.caption(null) }).catch(() => {})
  await sleep(250)
  await page.screenshot({ path: p })
  if (current) await caption(current)
  log('still', p)
}

// Background: approve plan cards the story does not click itself, and never
// let a per-tool card pass silently. The builder's tools that act on its own
// draft are covered by the stage's approval; the ones that ask a person by
// design (an install request, a credential ask) raise one card each. Every
// per-tool card is counted and logged with its moment so the reader can tell
// the two apart from the session's records, then approved so the story goes on.
let autoApprove = true
let bgStop = false
let toolCards = 0
const bg = (async () => {
  while (!bgStop) {
    if (!autoApprove) { await sleep(1000); continue }
    try {
      for (const p of pending()) {
        if (p.category === 'plan_phase' || p.category === 'plan_amendment') {
          log('auto-approve', p.category, p.ref); await decide(p.category, p.ref)
        } else if (p.category === 'tool_approval') {
          toolCards++
          log('per-tool card approved off-screen (by design for an install or credential ask; a finding for a draft tool):', p.ref)
          await decide('tool_approval', p.ref)
        }
      }
    } catch (e) { log('bg poll errored:', e.message) }
    await sleep(4000)
  }
})()

const ctx = { page, base: BASE, ns: NS, cls: CLASS, sess, login, api, say, decide, pending, phase, waitFor, composed, steps, caption, pointerTo, click, dismissReplyModal, still, log, sleep, kubectl, setAutoApprove: (v) => { autoApprove = v } }

// Intro title card.
await page.goto(`${BASE}/sessions`, { waitUntil: 'networkidle' })
await login()
await caption(story.intro || 'Building an agent with the agent builder.')
{ const from = Date.now() - T0; await sleep(3500); kept.push([from, Date.now() - T0]) }

for (const beat of story.beats) {
  log('beat', beat.id)
  await caption(beat.caption ?? null)
  const runStart = Date.now() - T0
  // A beat that gives up leaves a still of what the page showed, so the
  // failure can be read without re-running the whole story.
  try { await beat.run(ctx) } catch (e) { await still(`_failed-${beat.id}`).catch(() => {}); throw e }
  const from = Math.max(0, (beat.keepFrom === 'run' ? runStart : Date.now() - T0) - 1200)
  await sleep((beat.hold ?? 5) * 1000)
  if (beat.shot) await still(beat.shot)
  kept.push([from, Date.now() - T0])
}
await caption(null)
bgStop = true
await bg
const rawPath = await page.video().path()
await context.close()
await browser.close()

// ffmpeg: cut each kept window, concat, then webm (VP9) + mp4 (H.264) + poster.
const ff = (args, label) => {
  const r = spawnSync('ffmpeg', ['-y', '-loglevel', 'error', ...args], { stdio: 'inherit' })
  if (r.status !== 0) throw new Error(`ffmpeg ${label} failed (${r.status})`)
}
const segs = []
kept.forEach(([a, b], i) => {
  const seg = path.join(rawDir, `seg-${path.basename(outBase)}-${i}.webm`)
  ff(['-ss', (a / 1000).toFixed(2), '-to', (b / 1000).toFixed(2), '-i', rawPath, '-an', '-c:v', 'libvpx-vp9', '-b:v', '0', '-crf', '32', seg], `seg ${i}`)
  segs.push(seg)
})
const list = path.join(rawDir, `concat-${path.basename(outBase)}.txt`)
fs.writeFileSync(list, segs.map((s) => `file '${path.resolve(s)}'`).join('\n'))
const webmOut = `${outBase}.webm`, mp4Out = `${outBase}.mp4`, posterOut = `${outBase}.png`
ff(['-f', 'concat', '-safe', '0', '-i', list, '-c', 'copy', webmOut], 'concat')
ff(['-i', webmOut, '-an', '-c:v', 'libx264', '-pix_fmt', 'yuv420p', '-crf', '23', '-movflags', '+faststart', mp4Out], 'mp4')
ff(['-ss', '1', '-i', webmOut, '-frames:v', '1', posterOut], 'poster')
log(`wrote:\n  ${webmOut}\n  ${mp4Out}\n  ${posterOut}\n  stills in ${SHOTS}\n(raw: ${rawPath}; session ${sess.ns}/${sess.name} left for the caller to clean up)`)
if (toolCards) log(`NOTE: ${toolCards} per-tool card(s) were approved off-screen; read the session's plan-gate and authorization records before deleting it`)
