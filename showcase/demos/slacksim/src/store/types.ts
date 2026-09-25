import type { Attachment, Block } from '../blockkit/types'

// The in-memory model the fake Slack renders from. It mirrors the parts of
// Slack's data model OAP actually uses: a workspace, users (human + agent/bot),
// channels/DMs organised into sidebar sections, and threaded messages whose body
// is either Slack mrkdwn or Block Kit JSON. Everything here is deterministic —
// timestamps come from a frozen clock, ids are author-assigned — so captures are
// byte-stable.

export interface Workspace {
  id: string
  name: string
  /** Single-letter/emoji shown in the workspace rail icon. */
  glyph: string
  /** Hex accent behind the workspace glyph. */
  accent: string
}

export interface Avatar {
  /** 1–2 initials shown when there is no image. */
  initials: string
  /** Hex background for the initials tile. */
  color: string
  /** Optional image URL (data: or /public path); overrides initials. */
  url?: string
}

export interface SimUser {
  id: string
  name: string
  avatar: Avatar
  /** True for apps/agents — renders the small square avatar + APP/AGENT tag. */
  isBot?: boolean
  /** Badge text next to a bot name: usually "APP", customized to "AGENT" by OAP. */
  botBadge?: string
  presence?: 'active' | 'away'
}

export type SidebarSection = 'starred' | 'channels' | 'dms' | 'apps'

export interface SimChannel {
  id: string
  /** Display name without the leading # (channels) — the raw handle. */
  name: string
  kind: 'channel' | 'private' | 'dm' | 'app'
  section: SidebarSection
  topic?: string
  /** For a DM/app conversation, the other participant user ids. */
  memberIds?: string[]
  /** Sidebar emphasis. */
  unread?: boolean
  mentionCount?: number
  starred?: boolean
  /** For kind==='app': the App Home tab view, as Block Kit (app_home_view). */
  home?: Block[]
}

export interface Reaction {
  /** Emoji shortcode without colons, e.g. "white_check_mark". */
  name: string
  count: number
}

export interface SimMessage {
  id: string
  channelId: string
  userId: string
  /** Slack timestamp string "1699999999.000100"; also the message identity. */
  ts: string
  /** Present on a threaded reply — the parent message ts. */
  threadTs?: string
  /** Body as Slack mrkdwn. Ignored when `blocks` is set. */
  text?: string
  /** Body as Block Kit JSON (from the capture harness or hand-authored). */
  blocks?: Block[]
  /** Legacy attachments (the colored severity bar). */
  attachments?: Attachment[]
  /** System lines rendered muted (joins, participant notices). */
  subtype?: 'channel_join' | 'thread_join' | 'me_message' | 'notice'
  reactions?: Reaction[]
  /** Thread rollup shown on the parent message in the main pane. */
  replyCount?: number
  replyUserIds?: string[]
  lastReplyTs?: string
}

/** Which surface the message pane is showing. */
export interface ViewState {
  activeChannelId: string
  /** When set, the thread side-panel is open on this parent ts. */
  openThreadTs?: string
  /** Per-thread assistant status caption (Slack assistant.threads.setStatus). */
  assistantStatus?: Record<string, string | undefined>
  /** userId currently shown as "typing…" in the active surface. */
  typingUserId?: string
  /** App Home is open on this app's channel id (kind==='app'). */
  appHomeChannelId?: string
}

export interface Scenario {
  workspace: Workspace
  /** The signed-in user (drives "you"/right-aligned nothing; Slack left-aligns all). */
  meId: string
  users: SimUser[]
  channels: SimChannel[]
  messages: SimMessage[]
  view: ViewState
}
