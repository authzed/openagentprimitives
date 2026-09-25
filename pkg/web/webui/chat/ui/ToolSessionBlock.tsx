import { useState } from "react";
import { ChevronDown, ChevronRight, CircleDot, Terminal } from "lucide-react";
import { cn } from "@ap/design";
import type { AnsiSpan, ToolSessionLine, ToolSessionState } from "./toolSession";

// AnsiText renders one parsed line as styled spans.
//
// Colours arrive as inline styles because they are per-span RGB values decoded
// from the tool's own escape codes — there is no finite Tailwind class set to
// map them onto. Crucially the CONTENT is still React text, never
// dangerouslySetInnerHTML: this is untrusted tool output, so markup in it must
// render as literal characters and never as DOM.
//
// The inline style="" only works because the chat is an ordinary webui document,
// whose CSP is style-src 'self' 'unsafe-inline' (pkg/web/webui/document.go — already
// required for the styles Radix/shadcn inject at runtime). Do NOT reuse this
// component on the artifact-view host: that page runs style-src 'nonce-X' with
// no unsafe-inline (pkg/web/webui/artifactview/host.go), which never covers inline
// style="", so every colour here would be silently dropped. The artifact view
// has its own ChatPanel and does not import this.
function AnsiText({ spans }: { spans: AnsiSpan[] }) {
  return (
    <>
      {spans.map((s, i) => {
        const style: React.CSSProperties = {};
        if (s.fg) style.color = s.fg;
        if (s.bg) style.backgroundColor = s.bg;
        if (s.dim) style.opacity = 0.6;
        return (
          <span
            key={i}
            style={style}
            className={cn(s.bold && "font-semibold", s.italic && "italic", s.underline && "underline")}
          >
            {s.text}
          </span>
        );
      })}
    </>
  );
}

// TranscriptLine renders one line. data-stream carries the origin so stderr is
// distinguishable from stdout without relying on colour alone.
function TranscriptLine({ line }: { line: ToolSessionLine }) {
  return (
    <div
      data-stream={line.kind}
      className={cn(
        "whitespace-pre-wrap break-all font-mono text-[11px] leading-[1.45]",
        line.kind === "stderr" && "text-red-300/90",
        line.kind === "event" && "text-foreground/80",
      )}
    >
      {line.toolName && (
        <span className="mr-1.5 inline-flex items-center gap-1 text-[10px] text-muted-foreground">
          <CircleDot className="h-2.5 w-2.5" />
          {line.toolName}
        </span>
      )}
      <AnsiText spans={line.spans} />
    </div>
  );
}

// formatDuration renders a millisecond duration as "18.2s" / "1m18s".
function formatDuration(ms: number): string {
  const secs = ms / 1000;
  if (secs < 60) return `${secs.toFixed(1)}s`;
  const m = Math.floor(secs / 60);
  return `${m}m${Math.round(secs - m * 60)}s`;
}

// resultSummary describes a finished session: outcome, then whichever of
// duration / cost / exit detail the payload actually carried. Never invents a
// field that was not sent.
function resultSummary(state: ToolSessionState): string {
  const parts: string[] = [state.ok ? "done" : "failed"];
  if (!state.ok && state.exitCode !== undefined) parts.push(`exit ${state.exitCode}`);
  if (!state.ok && state.exitReason && state.exitReason !== "failed") parts.push(state.exitReason);
  if (state.durationMs !== undefined) parts.push(formatDuration(state.durationMs));
  if (state.costUsd !== undefined) parts.push(`$${state.costUsd.toFixed(2)}`);
  return parts.join(" · ");
}

// ToolSessionBlock renders one interactive tool call's live transcript — the
// web-chat counterpart of Slack's per-ToolCallRef Block Kit message
// (pkg/channels/channelkinds/slack/sender_tool_session.go).
//
// Purely presentational: ChatView owns the state and the frame folding (see
// toolSession.ts), mirroring how OperationTree is fed by ChatView. The only
// local state is the expand/collapse toggle, which is a per-viewer UI
// preference and belongs nowhere else.
export function ToolSessionBlock({ state }: { state: ToolSessionState }) {
  // A finished, successful run is noise in a long conversation. A FAILED one is
  // precisely what the user wants to read, so it stays open. `undefined` means
  // "follow that default"; an explicit boolean means the user has decided.
  const [override, setOverride] = useState<boolean | undefined>(undefined);
  const expanded = override ?? !(state.done && state.ok);

  const header = [state.outerTool || "tool", state.reason].filter(Boolean).join(" · ");

  return (
    <div className="my-1 w-full overflow-hidden rounded-md border border-border/60 bg-background/40">
      <button
        type="button"
        data-testid="tool-session-toggle"
        onClick={() => setOverride(!expanded)}
        className="flex w-full items-center gap-1.5 px-2 py-1.5 text-left text-[11px] text-muted-foreground hover:bg-foreground/5"
      >
        {expanded ? <ChevronDown className="h-3 w-3 shrink-0" /> : <ChevronRight className="h-3 w-3 shrink-0" />}
        <Terminal className="h-3 w-3 shrink-0" />
        <span className={cn("truncate", !state.done && "animate-pulse")}>{header}</span>
      </button>

      {expanded && (
        <div data-testid="tool-session-body" className="max-h-80 overflow-auto border-t border-border/60 px-2 py-1.5">
          {state.elided > 0 && (
            <div className="mb-1 text-[10px] italic text-muted-foreground/70">
              … {state.elided} earlier {state.elided === 1 ? "line" : "lines"} elided
            </div>
          )}
          {state.lines.map((line, i) => (
            <TranscriptLine key={i} line={line} />
          ))}
        </div>
      )}

      {state.done && (
        <div
          data-testid="tool-session-footer"
          className={cn(
            "border-t border-border/60 px-2 py-1 text-[10px]",
            state.ok ? "text-muted-foreground" : "text-red-300/90",
          )}
        >
          {resultSummary(state)}
        </div>
      )}
    </div>
  );
}
