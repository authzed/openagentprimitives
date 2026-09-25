import { useCallback, useEffect, useRef, useState } from "react";

// notificationsSupported reports whether the Web Notifications API exists —
// absent in jsdom and older browsers. Every use is guarded by it, so the hook
// degrades to the title-badge-only path (unread still counts) rather than
// throwing.
function notificationsSupported(): boolean {
  return typeof window !== "undefined" && "Notification" in window;
}

// useAwayNotifications tracks replies that arrive while the tab is inactive.
// It raises an OS-level Web Notification (permission-gated, gesture-armed) and
// exposes an `unread` count for the tab-title badge. `active` is the current
// tab-activity (see useTabActivity).
export function useAwayNotifications(active: boolean): {
  unread: number;
  notify: (title: string, body: string) => void;
  requestPermission: () => void;
} {
  const [unread, setUnread] = useState(0);

  // The user is looking again — drop the unread badge.
  useEffect(() => {
    if (active) setUnread(0);
  }, [active]);

  // Ask for permission once, and only from a user gesture (the first send —
  // browsers reject requestPermission() outside a gesture, and prompting on
  // load is an anti-pattern). asked de-dupes across renders.
  const asked = useRef(false);
  const requestPermission = useCallback(() => {
    if (asked.current || !notificationsSupported()) return;
    asked.current = true;
    if (Notification.permission === "default") void Notification.requestPermission();
  }, []);

  const notify = useCallback(
    (title: string, body: string) => {
      if (active) return; // visible + focused → the reply is already on screen
      setUnread((n) => n + 1);
      if (!notificationsSupported() || Notification.permission !== "granted") return;
      try {
        const n = new Notification(title, { body });
        n.onclick = () => {
          window.focus();
          n.close();
        };
      } catch {
        // Some browsers throw when a Notification is constructed without a
        // service worker (e.g. mobile Chrome). The title badge already
        // reflects the unread count, so this degrades cleanly.
      }
    },
    [active],
  );

  return { unread, notify, requestPermission };
}
