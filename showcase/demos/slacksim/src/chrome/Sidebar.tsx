import { ChevronDown, Hash, Lock, Search, Edit, MessageSquareText, Headphones, Send } from 'lucide-react'
import { useLookups, useScenario, useStore } from '../runtime/context'
import type { SidebarSection, SimChannel } from '../store/types'
import { Avatar } from './Avatar'

const SECTION_LABELS: Record<SidebarSection, string> = {
  starred: 'Starred',
  channels: 'Channels',
  dms: 'Direct messages',
  apps: 'Agents & apps',
}
const SECTION_ORDER: SidebarSection[] = ['starred', 'channels', 'dms', 'apps']

export function Sidebar() {
  const scenario = useScenario()
  const store = useStore()
  const { workspace, channels, view } = scenario

  const bySection = (s: SidebarSection) => channels.filter((c) => c.section === s)

  return (
    <div className="sk-sidebar">
      <header className="sk-sidebar-head">
        <button className="sk-ws-switcher">
          <span className="sk-ws-name">{workspace.name}</span>
          <ChevronDown size={18} />
        </button>
        <button className="sk-icon-btn" aria-label="New message">
          <Edit size={18} />
        </button>
      </header>

      <div className="sk-sidebar-search">
        <Search size={15} />
        <span className="sk-search-placeholder">Find a conversation…</span>
      </div>

      <nav className="sk-sidebar-scroll">
        <ul className="sk-nav-list">
          <NavItem icon={<MessageSquareText size={18} />} label="Threads" />
          <NavItem icon={<Headphones size={18} />} label="Huddles" />
          <NavItem icon={<Send size={18} />} label="Drafts & sent" />
        </ul>

        {SECTION_ORDER.map((section) => {
          const items = bySection(section)
          if (items.length === 0) return null
          return (
            <section className="sk-sidebar-section" key={section}>
              <div className="sk-section-header">
                <ChevronDown size={14} />
                <span>{SECTION_LABELS[section]}</span>
              </div>
              <ul className="sk-channel-list">
                {items.map((c) => (
                  <ChannelItem
                    key={c.id}
                    channel={c}
                    active={view.activeChannelId === c.id}
                    onClick={() => (c.kind === 'app' ? store.appHome(c.id) : store.switchChannel(c.id))}
                  />
                ))}
              </ul>
            </section>
          )
        })}
      </nav>
    </div>
  )
}

function NavItem({ icon, label }: { icon: React.ReactNode; label: string }) {
  return (
    <li className="sk-nav-item">
      <span className="sk-nav-icon">{icon}</span>
      <span className="sk-nav-label">{label}</span>
    </li>
  )
}

function ChannelItem({
  channel,
  active,
  onClick,
}: {
  channel: SimChannel
  active: boolean
  onClick: () => void
}) {
  const { user } = useLookups()
  const cls = `sk-channel-item ${active ? 'is-active' : ''} ${channel.unread ? 'is-unread' : ''}`.trim()

  let icon: React.ReactNode
  let label = channel.name
  if (channel.kind === 'private') icon = <Lock size={15} />
  else if (channel.kind === 'channel') icon = <Hash size={15} />
  else if (channel.kind === 'app') {
    const u = user(channel.memberIds?.[0] ?? '')
    icon = <Avatar user={u} size={20} />
    label = u?.name ?? channel.name
  } else {
    // DM: show the other member's avatar + presence
    const other = user(channel.memberIds?.[0] ?? '')
    icon = (
      <span className="sk-dm-avatar">
        <Avatar user={other} size={20} />
        <span className={`sk-presence ${other?.presence === 'active' ? 'is-active' : ''}`} />
      </span>
    )
    label = channel.memberIds?.map((id) => user(id)?.name ?? id).join(', ') ?? channel.name
  }

  return (
    <li>
      <button className={cls} onClick={onClick} data-channel={channel.id}>
        <span className="sk-channel-icon">{icon}</span>
        <span className="sk-channel-name">{label}</span>
        {channel.mentionCount ? <span className="sk-mention-badge">{channel.mentionCount}</span> : null}
      </button>
    </li>
  )
}
