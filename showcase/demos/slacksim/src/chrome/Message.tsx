import { Blocks, Attachments } from '../blockkit/BlockKit'
import { Mrkdwn } from '../blockkit/mrkdwn'
import { useLookups } from '../runtime/context'
import { formatTime } from '../runtime/time'
import type { Reaction, SimMessage } from '../store/types'
import { Avatar } from './Avatar'

const EMOJI_GLYPH: Record<string, string> = {
  white_check_mark: '✅',
  heavy_check_mark: '✔️',
  eyes: '👀',
  tada: '🎉',
  rocket: '🚀',
  '+1': '👍',
  fire: '🔥',
}

export function MessageBody({ message }: { message: SimMessage }) {
  const { mrkdwn } = useLookups()
  const isSystem =
    message.subtype === 'channel_join' || message.subtype === 'thread_join' || message.subtype === 'notice'
  return (
    <div className={`sk-msg-body ${isSystem ? 'sk-msg-body--system' : ''}`.trim()}>
      {message.blocks ? (
        <Blocks blocks={message.blocks} ctx={mrkdwn} />
      ) : message.text ? (
        <div className="sk-msg-text">
          <Mrkdwn text={message.text} ctx={mrkdwn} />
        </div>
      ) : null}
      {message.attachments && message.attachments.length > 0 && (
        <Attachments attachments={message.attachments} ctx={mrkdwn} />
      )}
      {message.reactions && message.reactions.length > 0 && <Reactions reactions={message.reactions} />}
    </div>
  )
}

function Reactions({ reactions }: { reactions: Reaction[] }) {
  return (
    <div className="sk-reactions">
      {reactions.map((r) => (
        <button className="sk-reaction" key={r.name}>
          <span className="sk-reaction-emoji">{EMOJI_GLYPH[r.name] ?? `:${r.name}:`}</span>
          <span className="sk-reaction-count">{r.count}</span>
        </button>
      ))}
    </div>
  )
}

function ThreadRollup({ message, onOpen }: { message: SimMessage; onOpen: () => void }) {
  const { user } = useLookups()
  const repliers = (message.replyUserIds ?? []).slice(0, 5)
  return (
    <button className="sk-thread-rollup" onClick={onOpen}>
      <span className="sk-thread-avatars">
        {repliers.map((id) => (
          <Avatar key={id} user={user(id)} size={20} />
        ))}
      </span>
      <span className="sk-thread-count">
        {message.replyCount} {message.replyCount === 1 ? 'reply' : 'replies'}
      </span>
      <span className="sk-thread-lastreply">View thread</span>
    </button>
  )
}

export function MessageRow({
  message,
  grouped,
  onOpenThread,
  showRollup = true,
}: {
  message: SimMessage
  grouped: boolean
  onOpenThread?: (parentTs: string) => void
  showRollup?: boolean
}) {
  const { user } = useLookups()
  const u = user(message.userId)

  return (
    <div className={`sk-msg ${grouped ? 'is-grouped' : ''}`.trim()} data-ts={message.ts}>
      <div className="sk-msg-gutter">
        {grouped ? (
          <span className="sk-msg-hovertime">{formatTime(message.ts)}</span>
        ) : (
          <Avatar user={u} size={36} />
        )}
      </div>
      <div className="sk-msg-main">
        {!grouped && (
          <div className="sk-msg-head">
            <span className="sk-msg-author">{u?.name ?? message.userId}</span>
            {u?.isBot && <span className="sk-app-badge">{u.botBadge ?? 'APP'}</span>}
            <span className="sk-msg-time">{formatTime(message.ts)}</span>
          </div>
        )}
        <MessageBody message={message} />
        {showRollup && message.replyCount && onOpenThread ? (
          <ThreadRollup message={message} onOpen={() => onOpenThread(message.ts)} />
        ) : null}
      </div>
    </div>
  )
}
