import { newScenario } from '../store/scenario'
import type { Story } from '../runtime/story'
import type { Block } from '../blockkit/types'
import infoLeakage from '../fixtures/blockkit/info-leakage.json'

// Information leakage (per-datum egress): the agent drafted a reply carrying a
// company's data, but a guest in the channel isn't permitted to see it — so the
// DATA owner (Jordan), not the requester, must approve the share. The card is
// OAP's real info_leakage Block Kit (captured by `mage blocks:capture`): the
// proposed share, "Would share with @jamie", routed to the data owner. All
// identifiers are fabricated.

const OWNER = 'U_OWNER' // jordan — Circldot's owner / "me"
const AGENT = 'U7' // crmbot
const DM = 'D1'
const CARD_TS = '1756700300.000000'
const blocks = infoLeakage as Block[]

export function infoLeakageDemo(): Story {
  const s = newScenario()
    .workspace('Acme Robotics', { glyph: 'A', accent: '#4a154b' })
    .at('2026-08-30T16:00:00Z')
    .user(OWNER, 'jordan')
    .user('U_GUEST', 'jamie')
    .bot(AGENT, 'crmbot', { badge: 'AGENT' })
    .me(OWNER)
    .channel('C1', 'sales', { starred: true })
    .dm(DM, [AGENT])

  const scenario = s.open(DM).build()

  return {
    scenario,
    intro: 'Information leakage: sharing data with someone not permitted needs the data owner’s approval.',
    beats: [
      {
        id: 'card',
        caption:
          'A guest in the channel can’t see Circldot’s data, so before the reply goes out crmbot asks the *data owner* to approve the share.',
        hold: 1000,
        run: (c) => {
          c.postMessage(DM, AGENT, { blocks, ts: CARD_TS })
          c.switchChannel(DM)
        },
      },
    ],
  }
}
