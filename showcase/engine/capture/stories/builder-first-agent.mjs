// Story: a person builds their first custom agent with the agent builder, on
// the real product. Played by engine/capture/webclip.mjs against a running OAP
// web server whose cluster has the builder installed. Every beat waits for the
// real page to reach the state its caption describes; the model's thinking time
// between beats is cut from the clip.
//
// Selectors are the product's own visible labels. Where a label is not stable
// enough to click, the beat falls back to the same HTTP call the page makes.
const DESCRIPTION =
  'I want a small agent for my team: it replies to any message with a short, friendly limerick about it. ' +
  'It needs no tools and no accounts, only we will use it, in chat.'

export const intro = 'Building your first custom agent with the agent builder — a conversation, not a config file.'

const has = async (loc) => (await loc.count()) > 0

// The builder asks one question at a time and parks until it is answered. While
// a beat waits for a later state, any question it puts on the page is answered
// with a confirmation — every question in this build is a "did I get that
// right?" — so the story never sits on a parked builder. Rate-limited so one
// answer is not sent twice for one question.
let lastNudge = 0
// A hook the builder painted and later cleared keeps its composed mark, so
// "is it asking?" reads the page for an actual question, not the mark.
const asking = (c) => c.page.$eval('[data-hook="questions"]', (e) => (e.innerText || '').trim().length > 0).catch(() => false)
const nudge = async (c) => {
  if (Date.now() - lastNudge < 45000) return
  if (!(await asking(c))) return
  lastNudge = Date.now()
  c.log('answering the question on the page')
  await c.say("Yes, that's right — go ahead with sensible defaults for everything else.")
}

export const beats = [
  {
    id: 'newsession',
    caption: 'From the sessions page, pick the Agent Builder like any other agent.',
    hold: 4,
    shot: 'builder-newsession',
    run: async (c) => {
      await c.page.goto(`${c.base}/sessions`, { waitUntil: 'networkidle' })
      await c.login()
      await c.click(c.page.locator('button:has-text("New session")').first())
      await c.page.waitForTimeout(900)
      const sel = c.page.locator('select').first()
      await c.pointerTo(sel)
      await sel.selectOption({ label: "Agent Builder" })
      await c.sleep(600)
      const start = c.page.locator('button:has-text("Open UI"), button:has-text("Start session")').first()
      if (await start.isDisabled().catch(() => false)) {
        c.log('start is disabled with an empty message; the class needs one — filling a short one')
        await c.page.locator('textarea').first().fill("Let's build an agent.")
      }
    },
  },
  {
    id: 'open',
    caption: 'The workshop page opens with one question: what should this agent do?',
    hold: 4,
    shot: 'builder-intake',
    run: async (c) => {
      await c.click(c.page.locator('button:has-text("Open UI"), button:has-text("Start session")').first())
      await c.page.waitForURL(/session=/, { timeout: 60000 })
      const m = decodeURIComponent(new URL(c.page.url()).searchParams.get('session') || '').split('/')
      c.sess.ns = m[0] || c.ns
      c.sess.name = m[1] || ''
      c.log('session', c.sess.ns, c.sess.name)
      await c.page.locator('form textarea').first().waitFor({ timeout: 120000 })
      await c.sleep(1500)
    },
  },
  {
    id: 'describe',
    caption: 'Say it in your own words. No schema, no fields — the builder asks about anything it still needs.',
    hold: 2,
    keepFrom: 'run',
    run: async (c) => {
      const ta = c.page.locator('form textarea').first()
      // The form is disabled while the builder's opening turn runs, and the
      // shell may pop its reply modal over it; wait for both to clear.
      await c.waitFor(async () => { await c.dismissReplyModal(); return !(await ta.isDisabled().catch(() => true)) }, { what: 'the describe form to accept input', timeoutMs: 120000, everyMs: 1500 })
      await c.click(ta)
      await ta.type(DESCRIPTION, { delay: 18 })
      await c.sleep(800)
      await c.click(c.page.locator('button:has-text("Start building")').first())
    },
  },
  {
    id: 'questions',
    caption: 'It restates what it understood, records what is settled, and asks one question at a time.',
    hold: 6,
    shot: 'builder-questions',
    run: async (c) => {
      let probes = 0
      await c.waitFor(async () => {
        const hooks = await c.composed()
        if (++probes % 5 === 1) c.log('painted hooks so far:', hooks.join(',') || '(none)')
        return hooks.includes('questions')
      }, { what: 'the first question', timeoutMs: 300000 })
      await c.sleep(1500)
    },
  },
  {
    id: 'answer',
    caption: 'Answer on the page or in the chat. Each stage runs into the next on its own.',
    hold: 3,
    run: async (c) => {
      c.setAutoApprove(false) // the next plan card is the one the story clicks itself
      lastNudge = Date.now()
      await c.say('Yes, exactly. Use sensible defaults for everything else and go ahead.')
      await c.waitFor(async () => { await nudge(c); const s = await c.steps(); return s.assess === 'done' }, { what: 'the intake stage to finish', timeoutMs: 300000 })
    },
  },
  {
    id: 'approve',
    caption: 'Before it changes the draft, you approve the stage — once. Every change in that stage then runs without another card.',
    hold: 4,
    shot: 'builder-approve',
    run: async (c) => {
      await c.waitFor(async () => { if (c.pending().some((p) => p.category === 'plan_phase')) return true; await nudge(c); return false }, { what: 'the stage card', timeoutMs: 420000, everyMs: 2500 })
      // The shell surfaces the pending card above the agent view, with its
      // Approve and Deny buttons — the same card the conversation view shows.
      // A message that reaches the builder just as it raises the card leaves
      // the page "picking up" that message with no card shown, while the
      // session still lists the card as pending; when that happens the story
      // decides through the same call the card makes and skips the still.
      const btn = c.page.getByRole('button', { name: 'Approve' }).first()
      const shown = await c.waitFor(() => btn.isVisible().catch(() => false), { what: 'the card to appear above the page', timeoutMs: 45000, everyMs: 1500 }).then(() => true).catch((e) => { c.log(e.message); return false })
      if (shown) {
        await btn.scrollIntoViewIfNeeded().catch(() => {})
        await c.sleep(1500)
        await c.still('builder-approve-card')
        await c.click(btn)
      } else {
        c.log('no visible Approve button; deciding through the same call the card makes')
        for (const p of c.pending()) if (p.category === 'plan_phase') await c.decide(p.category, p.ref)
      }
      c.setAutoApprove(true)
      await c.sleep(2500)
    },
  },
  {
    id: 'build',
    caption: 'It composes the agent, applies it, and keeps fixing it until the platform reports it valid.',
    hold: 6,
    run: async (c) => {
      await c.waitFor(async () => { await nudge(c); return (await c.steps()).build === 'done' }, { what: 'the build stage to finish', timeoutMs: 600000 })
      await c.sleep(1500)
      // The rail shows one stage's panels at a time and follows the active
      // stage; a finished stage stays reachable. Look back at what Permissions
      // and Build settled before moving on — each still is taken with its
      // stage selected, so the panel it shows is that stage's.
      const step = (label) => c.page.locator(`button:has-text("${label}")`).first()
      if (await has(step('Permissions'))) { await c.click(step('Permissions')); await c.sleep(1500); await c.still('builder-permissions') }
      if (await has(step('Build'))) { await c.click(step('Build')); await c.sleep(1500); await c.still('builder-built') }
      // Selecting a step pins the rail; hand it back to the stage the builder is
      // on, or the Test panel never renders.
      if (await has(step('Test'))) { await c.click(step('Test')); await c.sleep(800) }
    },
  },
  {
    id: 'test',
    caption: 'Then you test it live, right on the page — a real session of the agent you just built.',
    hold: 4,
    shot: 'builder-test',
    run: async (c) => {
      const testStep = c.page.locator('button:has-text("Test")').first()
      await c.waitFor(async () => {
        if (await has(testStep)) await testStep.click().catch(() => {})
        if ((await c.composed()).includes('testRun')) return true
        await nudge(c); return false
      }, { what: 'the test card', timeoutMs: 300000 })
      // The card's start control is the embed button; its label is the
      // builder's own words ("Start the test", "Try it as yourself"), so it is
      // found by what it is, not by what it says. The builder may also park on
      // a question first, so the wait keeps answering.
      const link = c.page.locator('[data-hook="testRun"] [data-testid="ap-agentlink-embed"]').first()
      await c.waitFor(async () => { if (await link.isVisible().catch(() => false)) return true; await nudge(c); return false }, { what: 'the start-the-test button', timeoutMs: 300000, everyMs: 2000 })
      c.log('start button reads:', (await link.innerText().catch(() => '')).trim())
      await c.sleep(1200)
      await c.click(link)
      await c.page.getByTestId('ap-chat').first().waitFor({ timeout: 120000 })
      await c.sleep(3000)
      const frame = c.page.frameLocator('[data-testid="ap-chat"]').first()
      const composer = frame.getByPlaceholder('Message the agent…').first()
      if (await composer.count()) {
        await composer.click()
        await composer.type('It is raining today and I forgot my umbrella.', { delay: 20 })
        await composer.press('Enter')
        await c.sleep(30000)
      } else {
        c.log('no composer found in the embedded chat; leaving the test session idle')
        await c.sleep(4000)
      }
    },
  },
  {
    id: 'done',
    caption: 'Done testing? Say so. The builder reads what the agent actually did and asks if that looked right.',
    hold: 3,
    run: async (c) => {
      // The card's buttons are the builder's to paint; when it left them off,
      // saying the same thing in the conversation is what a person would do.
      const done = c.page.locator('button:has-text("Done testing")').first()
      if (await done.isVisible().catch(() => false)) {
        await c.click(done)
      } else {
        c.log('no Done testing button on the card; saying it in the chat instead')
        await c.say("I'm done testing — that looked right to me.")
      }
      lastNudge = Date.now()
      await c.waitFor(async () => (await asking(c)) || (await c.steps()).deliver === 'active', { what: 'the builder to ask about the test', timeoutMs: 300000 })
      await c.sleep(1500)
      if (await asking(c)) { lastNudge = Date.now(); await c.say('Yes, that looked right.') }
    },
  },
  {
    id: 'deliver',
    caption: 'It saves a portable copy of exactly what you tested — yours to keep — and offers to install it for real.',
    hold: 7,
    shot: 'builder-deliver',
    run: async (c) => {
      const deliverStep = c.page.locator('button:has-text("Deliver")').first()
      await c.waitFor(async () => {
        if ((await c.steps()).deliver !== 'upcoming' && await has(deliverStep)) await deliverStep.click().catch(() => {})
        return (await c.composed()).includes('deliver') && (await c.page.locator('button:has-text("Install for real")').count()) > 0
      }, { what: 'the delivery card', timeoutMs: 600000 })
      await c.sleep(2000)
    },
  },
  {
    id: 'install',
    caption: 'Installing is a platform admin’s decision. The request waits in the admin console until someone approves it.',
    hold: 5,
    shot: 'builder-install-requested',
    run: async (c) => {
      await c.click(c.page.locator('button:has-text("Install for real")').first())
      // The request lands on the workshop's spec; its status is written only
      // once an admin decides, so a pending request is spec-with-no-status.
      await c.waitFor(() => {
        const out = c.kubectl('-n', c.sess.ns, 'get', 'workshop', `${c.sess.name}-workshop`, '-o', 'jsonpath={.spec.installRequest.bundleDigest}')
        return out.trim() !== ''
      }, { what: 'the install request to be recorded', timeoutMs: 300000, everyMs: 3000 })
      await c.sleep(2500)
    },
  },
  {
    id: 'admin',
    caption: 'The admin console lists every open workshop, with the request waiting on it.',
    hold: 5,
    shot: 'admin-workshops',
    run: async (c) => {
      await c.page.goto(`${c.base}/admin/workshops`, { waitUntil: 'networkidle' })
      await c.sleep(800)
      await c.login()
      await c.page.goto(`${c.base}/admin/workshops`, { waitUntil: 'networkidle' }).catch(() => {})
      await c.page.getByRole('button', { name: 'Install' }).first().waitFor({ timeout: 60000 })
      await c.sleep(1500)
    },
  },
  {
    id: 'admin-install',
    caption: 'Install puts the draft, exactly as tested, into the namespace the admin chooses.',
    hold: 6,
    shot: 'admin-workshops-installed',
    run: async (c) => {
      await c.click(c.page.getByRole('button', { name: 'Install' }).first())
      const nsField = c.page.getByLabel('Namespace').first()
      await nsField.waitFor({ timeout: 30000 })
      await c.sleep(600)
      await nsField.fill('default')
      await c.sleep(800)
      await c.still('admin-workshops-install')
      const confirm = c.page.getByRole('button', { name: /Confirm install|Submit answers|Adopt and install/ }).first()
      await c.click(confirm)
      await c.waitFor(async () => (await c.page.getByText('Installed', { exact: false }).count()) > 0, { what: 'the row to show Installed', timeoutMs: 180000, everyMs: 2500 })
      await c.sleep(1500)
    },
  },
]
