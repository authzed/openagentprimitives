import { deriveRollups, type MessageBody } from "./scenario";
import type { Scenario, SimMessage } from "./types";
import type { OverlayControl } from "../runtime/overlay";

// A small observable store over a Scenario. Mutations are immutable (each
// produces a new Scenario reference) so React's useSyncExternalStore re-renders.
// The capture engine (Playwright) calls these methods mid-clip through
// `window.__showcase` to animate the conversation — post a message, open a
// thread, set the assistant status, show a typing indicator.
//
// Runtime timestamps continue a deterministic sequence seeded from the built
// message count, so a scripted sequence of driver calls is byte-stable.

export class SimStore {
  private scenario: Scenario;
  private listeners = new Set<() => void>();
  private cursorSecs: number;

  constructor(scenario: Scenario) {
    this.scenario = scenario;
    // Continue runtime timestamps from the last message in the built scenario,
    // so driver-posted messages stay on the same day (no date-divider jump).
    const maxSecs = scenario.messages.reduce(
      (m, msg) => Math.max(m, Math.floor(Number(msg.ts))),
      0,
    );
    this.cursorSecs = maxSecs || 1_756_500_000;
  }

  subscribe = (fn: () => void): (() => void) => {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  };

  getSnapshot = (): Scenario => this.scenario;

  private commit(next: Scenario): void {
    this.scenario = next;
    for (const fn of this.listeners) fn();
  }

  private nextTs(): string {
    // Advance from the last message so runtime posts stay on the same day.
    this.cursorSecs += 37;
    return `${this.cursorSecs}.000000`;
  }

  private appendMessage(msg: SimMessage): void {
    // Keep the runtime clock ahead of any explicitly-timestamped message so a
    // later auto-timestamped post never sorts before an earlier explicit one.
    this.cursorSecs = Math.max(this.cursorSecs, Math.floor(Number(msg.ts)));
    const messages = deriveRollups([...this.scenario.messages, msg]);
    this.commit({ ...this.scenario, messages });
  }

  // ---- driver API (window.__showcase) ------------------------------------

  postMessage(channelId: string, userId: string, body: MessageBody): string {
    const ts = body.ts ?? this.nextTs();
    this.appendMessage({
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
    return ts;
  }

  /** Update a message in place (Slack chat.update) — e.g. an approval card
   *  whose Approve/Deny buttons are replaced by an "Approved by …" line. */
  editMessage(
    channelId: string,
    ts: string,
    patch: Partial<
      Pick<SimMessage, "text" | "blocks" | "attachments" | "reactions">
    >,
  ): void {
    const messages = deriveRollups(
      this.scenario.messages.map((m) =>
        m.ts === ts && m.channelId === channelId ? { ...m, ...patch } : m,
      ),
    );
    this.commit({ ...this.scenario, messages });
  }

  postReply(
    channelId: string,
    parentTs: string,
    userId: string,
    body: MessageBody,
  ): string {
    const ts = body.ts ?? this.nextTs();
    this.appendMessage({
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
    return ts;
  }

  openThread(parentTs: string): void {
    this.commit({
      ...this.scenario,
      view: { ...this.scenario.view, openThreadTs: parentTs },
    });
  }

  closeThread(): void {
    this.commit({
      ...this.scenario,
      view: { ...this.scenario.view, openThreadTs: undefined },
    });
  }

  switchChannel(channelId: string): void {
    this.commit({
      ...this.scenario,
      view: {
        ...this.scenario.view,
        activeChannelId: channelId,
        openThreadTs: undefined,
        appHomeChannelId: undefined,
      },
    });
  }

  appHome(appId: string): void {
    this.commit({
      ...this.scenario,
      view: {
        ...this.scenario.view,
        activeChannelId: appId,
        appHomeChannelId: appId,
        openThreadTs: undefined,
      },
    });
  }

  /** Slack assistant.threads.setStatus — the "is thinking…" caption on a thread. */
  setStatus(threadTs: string, text: string | undefined): void {
    this.commit({
      ...this.scenario,
      view: {
        ...this.scenario.view,
        assistantStatus: {
          ...this.scenario.view.assistantStatus,
          [threadTs]: text,
        },
      },
    });
  }

  typing(userId: string | undefined): void {
    this.commit({
      ...this.scenario,
      view: { ...this.scenario.view, typingUserId: userId },
    });
  }
}

// The control surface the capture engine binds to. Kept minimal and stable —
// each method maps 1:1 to a screenplay `direction` verb. Store mutations plus
// the overlay controls (pointer / highlight / caption) are merged into one
// object so the engine has a single handle.
export interface ShowcaseControl extends OverlayControl {
  postMessage: SimStore["postMessage"];
  postReply: SimStore["postReply"];
  editMessage: SimStore["editMessage"];
  openThread: SimStore["openThread"];
  closeThread: SimStore["closeThread"];
  switchChannel: SimStore["switchChannel"];
  appHome: SimStore["appHome"];
  setStatus: SimStore["setStatus"];
  typing: SimStore["typing"];
}

declare global {
  interface Window {
    __showcase?: ShowcaseControl;
  }
}

export function bindControl(store: SimStore, overlay: OverlayControl): void {
  window.__showcase = {
    postMessage: store.postMessage.bind(store),
    postReply: store.postReply.bind(store),
    editMessage: store.editMessage.bind(store),
    openThread: store.openThread.bind(store),
    closeThread: store.closeThread.bind(store),
    switchChannel: store.switchChannel.bind(store),
    appHome: store.appHome.bind(store),
    setStatus: store.setStatus.bind(store),
    typing: store.typing.bind(store),
    ...overlay,
  };
}
