import { Star, Hash, Lock, Users, Headphones, ChevronDown, Bell, MoreVertical } from 'lucide-react'
import { useLookups } from '../runtime/context'
import type { SimChannel } from '../store/types'
import { Avatar } from './Avatar'

// The top bar of the message pane. Adapts to the channel kind: a regular/private
// channel shows the star + #name + member tools; an app shows the app avatar +
// Home/Messages/About tabs (the App Home surface).
export function ChannelHeader({ channel }: { channel: SimChannel }) {
  const { user } = useLookups()

  if (channel.kind === 'app') {
    const app = user(channel.memberIds?.[0] ?? '')
    return (
      <header className="sk-header sk-header--app">
        <div className="sk-header-title">
          <Star size={16} className="sk-header-star" />
          <Avatar user={app} size={20} />
          <span className="sk-header-name">{app?.name ?? channel.name}</span>
        </div>
        <nav className="sk-app-tabs">
          <button className="sk-app-tab is-active">Home</button>
          <button className="sk-app-tab">Messages</button>
          <button className="sk-app-tab">About</button>
        </nav>
      </header>
    )
  }

  const isDM = channel.kind === 'dm'
  const nameNode = isDM ? (
    <span className="sk-header-name">
      {channel.memberIds?.map((id) => user(id)?.name ?? id).join(', ')}
    </span>
  ) : (
    <span className="sk-header-name">{channel.name}</span>
  )

  return (
    <header className="sk-header">
      <div className="sk-header-title">
        {!isDM && <Star size={16} className="sk-header-star" />}
        {channel.kind === 'private' ? (
          <Lock size={15} />
        ) : channel.kind === 'channel' ? (
          <Hash size={16} />
        ) : null}
        {nameNode}
        <ChevronDown size={16} className="sk-header-caret" />
      </div>
      <div className="sk-header-tools">
        <button className="sk-header-pill">
          <Users size={15} />
          <span>5</span>
        </button>
        <button className="sk-header-pill">
          <Headphones size={15} />
          <ChevronDown size={13} />
        </button>
        <button className="sk-icon-btn" aria-label="Notifications">
          <Bell size={16} />
        </button>
        <button className="sk-icon-btn" aria-label="More">
          <MoreVertical size={16} />
        </button>
      </div>
    </header>
  )
}
