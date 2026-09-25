import * as React from "react";
import { CircleDot, CircleSlash, Hand } from "lucide-react";
import { relativeTime } from "./format";
import type { ListNotices, SessionRow } from "./types";

// sessionHref is the ONE place a row's link is built. Selection lives in the
// URL — never in this component's state — so following a row is a real
// navigation the server answers with a re-resolved `selected`, which is what
// makes a deep link and a click land on identical props.
export function sessionHref(ns: string, name: string): string {
  return `/sessions?session=${encodeURIComponent(`${ns}/${name}`)}`;
}

// noticeLines turns listNotices (pkg/web/webui/sessions/list.go) into its own
// lines of human copy. Each incompleteness is stated rather than swallowed:
// an empty list that is actually a truncated or partially-failed list would
// otherwise be indistinguishable from "you have no sessions", which is the
// silent-failure shape this repo forbids. The copy names what the viewer is
// missing and stays out of operational vocabulary — the operator log carries
// which of the three underlying causes it was.
function noticeLines(notices: ListNotices | undefined): string[] {
  const lines: string[] = [];
  if (!notices) return lines;
  if (notices.unavailable > 0) {
    lines.push(
      notices.unavailable === 1
        ? "1 session could not be loaded and is not shown."
        : `${notices.unavailable} sessions could not be loaded and are not shown.`,
    );
  }
  if (notices.truncated) lines.push("Showing your most recent sessions only.");
  // Stated for the same reason as the two above, about a different set: with
  // the bootstrap arm unevaluated the agent picker may be short entries this
  // viewer may in fact start, and an unexplained absence there reads as "you
  // have no agents" rather than "we could not check".
  if (notices.bootstrapUnavailable) {
    lines.push("Some agents may be missing from New chat. Try again in a moment.");
  }
  return lines;
}

export interface SessionListProps {
  sessions: readonly SessionRow[];
  notices?: ListNotices;
  // selected is the (ns, name) the URL currently names, or undefined on the
  // bare dashboard. Used only to mark the current row — the list never
  // decides what is selected, it only reflects the server's answer.
  selectedNs?: string;
  selectedName?: string;
}

// SessionList renders one row per session the viewer may interact with. It is
// a chrome region (SessionShell passes it to Chrome's `sidebar`), so nothing
// the content region renders can cover or remove it.
//
// Every row is an anchor, not a click handler: a session is an ADDRESS, so it
// must be openable in a new tab, copyable, and reachable by the back button.
// Rows are keyed by "<ns>/<name>", the same pair that identifies the session
// everywhere else, so a re-poll that reorders or replaces rows reconciles on
// identity rather than on position.
// sessionTag is the part of a session's object name that tells two sessions of
// the same class apart: the generated suffix after the class name. Shown only
// when two VISIBLE rows share a title, because an entry point that reads
// "chat-agent / chat-agent" is a history dump, and a hash on every row is
// noise the moment the titles differ.
export function sessionTag(name: string): string {
  const i = name.lastIndexOf("-");
  return i > 0 && i < name.length - 1 ? name.slice(i + 1) : name;
}

export function SessionList({ sessions, notices, selectedNs, selectedName }: SessionListProps) {
  const lines = noticeLines(notices);

  // Ended sessions are hidden by default: the list is the ENTRY POINT to the
  // product, and an entry point that leads with finished conversations reads
  // as an archive. The selected row is always shown, ended or not, so a deep
  // link never lands on a list that does not contain the thing it opened.
  // The toggle carries the count so nothing is silently missing.
  const [showEnded, setShowEnded] = React.useState(false);
  const isSelected = (s: SessionRow) => s.ns === selectedNs && s.name === selectedName;
  const hiddenEnded = sessions.filter((s) => s.ended && !isSelected(s)).length;
  const visible = showEnded ? sessions : sessions.filter((s) => !s.ended || isSelected(s));

  const titleCounts = new Map<string, number>();
  for (const s of visible) titleCounts.set(s.title, (titleCounts.get(s.title) ?? 0) + 1);

  return (
    <nav className="flex flex-col" aria-label="Your sessions" data-testid="session-shell-session-list">
      {lines.length > 0 && (
        <div className="px-3 py-2 text-[11px] text-muted-foreground border-b border-border" data-testid="session-shell-list-notices">
          {lines.map((line) => (
            <div key={line}>{line}</div>
          ))}
        </div>
      )}

      {sessions.length === 0 ? (
        <div className="px-3 py-4 text-[12px] text-muted-foreground" data-testid="session-shell-empty-list">
          You have no sessions yet.
        </div>
      ) : visible.length === 0 ? (
        <div className="px-3 py-4 text-[12px] text-muted-foreground" data-testid="session-shell-empty-list">
          No active sessions.
        </div>
      ) : (
        <ul className="flex flex-col">
          {visible.map((s) => {
            const current = isSelected(s);
            const duplicate = (titleCounts.get(s.title) ?? 0) > 1;
            // Computed once per row: relativeTime reads the clock, so calling
            // it for the guard and again for the value could straddle a bucket
            // boundary and render a label the guard did not agree to.
            const age = s.startedAt ? relativeTime(s.startedAt) : "";
            return (
              <li key={`${s.ns}/${s.name}`}>
                <a
                  href={sessionHref(s.ns, s.name)}
                  aria-current={current ? "page" : undefined}
                  data-testid={`session-shell-session-row-${s.ns}/${s.name}`}
                  className={`flex flex-col gap-0.5 px-3 py-2 border-b border-border hover:bg-surface-2 ${
                    current ? "bg-surface-active shadow-[inset_3px_0_0_hsl(var(--state))]" : ""
                  }`}
                >
                  <span className="flex items-baseline gap-1.5 min-w-0">
                    <span className="text-[13px] font-medium truncate">{s.title}</span>
                    {duplicate && (
                      <span
                        className="shrink-0 font-mono text-[10px] text-muted-foreground"
                        data-testid={`session-shell-session-row-tag-${s.ns}/${s.name}`}
                      >
                        {sessionTag(s.name)}
                      </span>
                    )}
                  </span>
                  <span className="flex items-center gap-1.5 text-[11px] text-muted-foreground">
                    {s.awaitingHuman ? (
                      <Hand className="h-3 w-3 shrink-0" aria-hidden="true" />
                    ) : s.ended ? (
                      <CircleSlash className="h-3 w-3 shrink-0" aria-hidden="true" />
                    ) : (
                      <CircleDot className="h-3 w-3 shrink-0" aria-hidden="true" />
                    )}
                    {/* phase is ALREADY human copy, mapped server-side
                        (list.go) — the raw control-plane phase never reaches
                        the browser, so this renders it verbatim rather than
                        re-deriving a second vocabulary here. */}
                    <span className="truncate" data-testid={`session-shell-session-row-phase-${s.ns}/${s.name}`}>
                      {s.phase}
                    </span>
                    {/* Recency, for telling two same-phase rows apart. Only
                        rendered when the session HAS a start time: startedAt
                        is optional on the wire (a session whose status has not
                        recorded one yet), and an empty slot says "not known"
                        honestly where "just now" would be a fabrication. */}
                    {age && (
                      <>
                        <span aria-hidden="true">·</span>
                        <span className="shrink-0" data-testid={`session-shell-session-row-age-${s.ns}/${s.name}`}>
                          {age}
                        </span>
                      </>
                    )}
                  </span>
                </a>
              </li>
            );
          })}
        </ul>
      )}

      {hiddenEnded > 0 || (showEnded && sessions.some((s) => s.ended)) ? (
        <button
          type="button"
          onClick={() => setShowEnded((v) => !v)}
          aria-expanded={showEnded}
          className="px-3 py-2 text-left text-[11px] text-muted-foreground hover:text-foreground hover:bg-surface-2"
          data-testid="session-shell-toggle-ended"
        >
          {showEnded ? "Hide ended sessions" : `Show ${hiddenEnded} ended session${hiddenEnded === 1 ? "" : "s"}`}
        </button>
      ) : null}
    </nav>
  );
}
