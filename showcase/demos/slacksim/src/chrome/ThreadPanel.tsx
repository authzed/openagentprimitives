import { useEffect, useRef } from 'react'
import { X, MoreVertical, ListChecks } from 'lucide-react'
import { useScenario } from '../runtime/context'
import { MessageRow } from './Message'
import { AssistantStatus } from './Indicators'
import { Composer } from './Composer'

// The right-hand thread panel. OAP's Slack integration is thread-centric, so a
// demo almost always ends up here: the parent "Picked up pull request…" message,
// the agent's threaded replies, and the assistant status caption while a turn
// runs (assistant.threads.setStatus).
export function ThreadPanel({ parentTs, onClose }: { parentTs: string; onClose: () => void }) {
  const scenario = useScenario()
  const parent = scenario.messages.find((m) => m.ts === parentTs)
  const replies = scenario.messages
    .filter((m) => m.threadTs === parentTs)
    .sort((a, b) => Number(a.ts) - Number(b.ts))
  const status = scenario.view.assistantStatus?.[parentTs]

  // Follow the newest reply, like a real Slack thread.
  const bodyRef = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const el = bodyRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [replies.length, status])

  if (!parent) return null

  return (
    <aside className="sk-thread">
      <header className="sk-thread-head">
        <span className="sk-thread-title">Thread</span>
        <div className="sk-thread-head-tools">
          <button className="sk-icon-btn" aria-label="Thread actions"><ListChecks size={16} /></button>
          <button className="sk-icon-btn" aria-label="More"><MoreVertical size={16} /></button>
          <button className="sk-icon-btn" aria-label="Close thread" onClick={onClose}><X size={18} /></button>
        </div>
      </header>

      <div className="sk-thread-body" ref={bodyRef}>
        <MessageRow message={parent} grouped={false} showRollup={false} />
        {replies.length > 0 && (
          <div className="sk-thread-replies-divider">
            <span>{replies.length} {replies.length === 1 ? 'reply' : 'replies'}</span>
            <span className="sk-thread-divider-line" />
          </div>
        )}
        {replies.map((m, i) => (
          <MessageRow
            key={m.ts}
            message={m}
            grouped={i > 0 && replies[i - 1].userId === m.userId}
            showRollup={false}
          />
        ))}
        {status && <AssistantStatus text={status} />}
      </div>

      <Composer placeholder="Reply…" />
    </aside>
  )
}
