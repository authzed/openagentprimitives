import { useLookups } from '../runtime/context'

// The assistant status caption (Slack assistant.threads.setStatus) — the subtle
// shimmering "… is thinking" line an AI app shows while a turn is running.
export function AssistantStatus({ text }: { text: string }) {
  return (
    <div className="sk-assistant-status" aria-live="polite">
      <span className="sk-shimmer">{text}</span>
      <span className="sk-typing-dots" aria-hidden>
        <span />
        <span />
        <span />
      </span>
    </div>
  )
}

export function TypingIndicator({ userId }: { userId: string }) {
  const { user } = useLookups()
  const name = user(userId)?.name ?? userId
  return (
    <div className="sk-typing">
      <span className="sk-typing-dots" aria-hidden>
        <span />
        <span />
        <span />
      </span>
      <span className="sk-typing-name">{name} is typing…</span>
    </div>
  )
}
