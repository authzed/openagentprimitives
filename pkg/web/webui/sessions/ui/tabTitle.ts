// formatTabTitle builds the browser-tab title for the chat app. The base
// "oap sessions" identifies the surface; an open conversation's title is
// prefixed before it, and an unread-reply count (replies that arrived while
// the tab was inactive — see useAwayNotifications) is shown as a leading
// "(N)" badge so a backgrounded tab advertises a waiting reply.
export function formatTabTitle(sessionTitle: string | null, unread: number): string {
  const base = "oap sessions";
  const core = sessionTitle ? `${sessionTitle} · ${base}` : base;
  return unread > 0 ? `(${unread}) ${core}` : core;
}
