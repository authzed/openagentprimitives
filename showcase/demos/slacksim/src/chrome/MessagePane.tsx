import { useEffect, useRef } from 'react'
import { useLookups, useScenario, useStore } from '../runtime/context'
import { dayKey, formatDateDivider } from '../runtime/time'
import type { SimChannel, SimMessage } from '../store/types'
import { ChannelHeader } from './ChannelHeader'
import { Composer } from './Composer'
import { MessageRow } from './Message'
import { TypingIndicator } from './Indicators'

export function MessagePane({ channel }: { channel: SimChannel }) {
  const scenario = useScenario()
  const store = useStore()
  const { user } = useLookups()

  const msgs = scenario.messages
    .filter((m) => m.channelId === channel.id && !m.threadTs)
    .sort((a, b) => Number(a.ts) - Number(b.ts))

  const placeholder =
    channel.kind === 'dm'
      ? channel.memberIds?.map((id) => user(id)?.name ?? id).join(', ') ?? channel.name
      : `#${channel.name}`

  const listRef = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const el = listRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [msgs.length, channel.id, scenario.view.typingUserId])

  return (
    <section className="sk-pane">
      <ChannelHeader channel={channel} />
      <div className="sk-msg-list" ref={listRef}>
        <div className="sk-msg-list-spacer" />
        {renderTimeline(msgs, store.openThread)}
        {scenario.view.typingUserId && <TypingIndicator userId={scenario.view.typingUserId} />}
      </div>
      <Composer placeholder={`Message ${placeholder}`} />
    </section>
  )
}

// Renders a flat message list into date-divided, author-grouped rows.
export function renderTimeline(msgs: SimMessage[], onOpenThread: (ts: string) => void) {
  const out: React.ReactNode[] = []
  let lastDay: string | null = null
  let lastAuthor: string | null = null

  for (const m of msgs) {
    const day = dayKey(m.ts)
    if (day !== lastDay) {
      out.push(
        <div className="sk-date-divider" key={`d-${m.ts}`}>
          <span className="sk-date-pill">{formatDateDivider(m.ts)}</span>
        </div>,
      )
      lastDay = day
      lastAuthor = null
    }
    const grouped = lastAuthor === m.userId && !m.subtype
    out.push(<MessageRow key={m.ts} message={m} grouped={grouped} onOpenThread={onOpenThread} />)
    lastAuthor = m.userId
  }
  return out
}
