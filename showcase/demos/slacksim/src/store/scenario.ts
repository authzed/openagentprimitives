import type { Attachment, Block } from "../blockkit/types";
import type {
  Reaction,
  Scenario,
  SidebarSection,
  SimChannel,
  SimMessage,
  SimUser,
  ViewState,
  Workspace,
} from "./types";

// A fluent, deterministic scenario builder (Klash's newScenario() analog).
// Timestamps come from a frozen clock seeded by .at(iso) and advanced by a fixed
// step per message, so two builds of the same script are byte-identical — the
// property captures depend on. No Date.now(), no randomness.

export interface MessageBody {
  text?: string;
  blocks?: Block[];
  attachments?: Attachment[];
  /** Explicit ts override; otherwise assigned from the frozen clock. */
  ts?: string;
  subtype?: SimMessage["subtype"];
  reactions?: Reaction[];
}

const DEFAULT_WORKSPACE: Workspace = {
  id: "W1",
  name: "Acme Robotics",
  glyph: "A",
  accent: "#611f69",
};

export class ScenarioBuilder {
  private ws: Workspace = { ...DEFAULT_WORKSPACE };
  private meId = "";
  private users: SimUser[] = [];
  private channels: SimChannel[] = [];
  private messages: SimMessage[] = [];
  private clockSecs = 1_760_000_000; // fixed default epoch; override with .at()
  private step = 0;
  private view: Partial<ViewState> = {};

  workspace(name: string, opts?: Partial<Omit<Workspace, "name">>): this {
    this.ws = { ...this.ws, name, ...opts };
    return this;
  }

  /** Freeze the clock at an ISO instant; message ts values derive from it. */
  at(iso: string): this {
    this.clockSecs = Math.floor(new Date(iso).getTime() / 1000);
    return this;
  }

  me(id: string): this {
    this.meId = id;
    return this;
  }

  user(
    id: string,
    name: string,
    opts?: Partial<Omit<SimUser, "id" | "name">>,
  ): this {
    this.users.push({
      id,
      name,
      avatar: opts?.avatar ?? {
        initials: initialsFor(name),
        color: colorFor(id),
      },
      presence: opts?.presence ?? "active",
      isBot: opts?.isBot,
      botBadge: opts?.botBadge,
    });
    return this;
  }

  /** An app/agent user: square avatar + APP/AGENT badge. */
  bot(
    id: string,
    name: string,
    opts?: { badge?: string; avatar?: SimUser["avatar"] },
  ): this {
    return this.user(id, name, {
      isBot: true,
      botBadge: opts?.badge ?? "APP",
      avatar: opts?.avatar ?? {
        initials: initialsFor(name),
        color: colorFor(id),
      },
    });
  }

  channel(
    id: string,
    name: string,
    opts?: {
      private?: boolean;
      section?: SidebarSection;
      topic?: string;
      unread?: boolean;
      mentionCount?: number;
      starred?: boolean;
    },
  ): this {
    this.channels.push({
      id,
      name,
      kind: opts?.private ? "private" : "channel",
      section: opts?.section ?? (opts?.starred ? "starred" : "channels"),
      topic: opts?.topic,
      unread: opts?.unread,
      mentionCount: opts?.mentionCount,
      starred: opts?.starred,
    });
    return this;
  }

  dm(
    id: string,
    memberIds: string[],
    opts?: { unread?: boolean; mentionCount?: number },
  ): this {
    this.channels.push({
      id,
      name: id,
      kind: "dm",
      section: "dms",
      memberIds,
      unread: opts?.unread,
      mentionCount: opts?.mentionCount,
    });
    return this;
  }

  /**
   * An app conversation (App Home surface). `home` is the Block Kit home tab;
   * `user` links the app to its bot user so the avatar resolves (defaults to id).
   */
  app(
    id: string,
    name: string,
    opts?: { unread?: boolean; home?: Block[]; user?: string },
  ): this {
    this.channels.push({
      id,
      name,
      kind: "app",
      section: "apps",
      memberIds: [opts?.user ?? id],
      unread: opts?.unread,
      home: opts?.home,
    });
    return this;
  }

  private nextTs(): string {
    const secs = this.clockSecs + this.step;
    this.step += 37;
    return `${secs}.000000`;
  }

  message(channelId: string, userId: string, body: MessageBody): this {
    const ts = body.ts ?? this.nextTs();
    this.messages.push({
      id: `${channelId}:${ts}`,
      channelId,
      userId,
      ts,
      text: body.text,
      blocks: body.blocks,
      attachments: body.attachments,
      subtype: body.subtype,
      reactions: body.reactions,
    });
    return this;
  }

  /** A threaded reply under an existing parent (identified by its ts). */
  reply(
    channelId: string,
    parentTs: string,
    userId: string,
    body: MessageBody,
  ): this {
    const ts = body.ts ?? this.nextTs();
    this.messages.push({
      id: `${channelId}:${ts}`,
      channelId,
      userId,
      ts,
      threadTs: parentTs,
      text: body.text,
      blocks: body.blocks,
      attachments: body.attachments,
      reactions: body.reactions,
    });
    return this;
  }

  open(channelId: string): this {
    this.view.activeChannelId = channelId;
    return this;
  }

  openThread(parentTs: string): this {
    this.view.openThreadTs = parentTs;
    return this;
  }

  appHome(appId: string): this {
    this.view.appHomeChannelId = appId;
    this.view.activeChannelId = appId;
    return this;
  }

  build(): Scenario {
    const messages = deriveRollups(this.messages);
    const activeChannelId =
      this.view.activeChannelId ?? this.channels[0]?.id ?? "";
    return {
      workspace: this.ws,
      meId: this.meId || this.users[0]?.id || "",
      users: this.users,
      channels: this.channels,
      messages,
      view: {
        activeChannelId,
        openThreadTs: this.view.openThreadTs,
        appHomeChannelId: this.view.appHomeChannelId,
        assistantStatus: {},
      },
    };
  }
}

export function newScenario(): ScenarioBuilder {
  return new ScenarioBuilder();
}

// Derive thread rollups (replyCount / replyUserIds / lastReplyTs) from the flat
// message list rather than storing them incrementally, so a reply added later —
// at build time or at runtime by the driver — can never desync a parent's
// rollup. Shared by the builder and the live store.
export function deriveRollups(messages: SimMessage[]): SimMessage[] {
  const repliesByParent = new Map<string, SimMessage[]>();
  for (const m of messages) {
    if (m.threadTs) {
      const arr = repliesByParent.get(m.threadTs) ?? [];
      arr.push(m);
      repliesByParent.set(m.threadTs, arr);
    }
  }
  return messages.map((m) => {
    if (m.threadTs) return m;
    const replies = repliesByParent.get(m.ts);
    if (!replies || replies.length === 0) return m;
    return {
      ...m,
      replyCount: replies.length,
      replyUserIds: uniq(replies.map((r) => r.userId)),
      lastReplyTs: replies[replies.length - 1].ts,
    };
  });
}

// ---- deterministic helpers ----------------------------------------------

function uniq<T>(xs: T[]): T[] {
  return [...new Set(xs)];
}

function initialsFor(name: string): string {
  const parts = name
    .trim()
    .split(/[\s._-]+/)
    .filter(Boolean);
  if (parts.length === 0) return "?";
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
  return (parts[0][0] + parts[1][0]).toUpperCase();
}

// A small fixed palette; the chosen color is a stable function of the id so the
// same user always gets the same avatar tint across builds.
const AVATAR_COLORS = [
  "#e8912d",
  "#4a154b",
  "#2eb67d",
  "#1264a3",
  "#e01e5a",
  "#616061",
  "#7c3aed",
  "#0b6e75",
];
function colorFor(id: string): string {
  let h = 0;
  for (let i = 0; i < id.length; i++) h = (h * 31 + id.charCodeAt(i)) >>> 0;
  return AVATAR_COLORS[h % AVATAR_COLORS.length];
}
