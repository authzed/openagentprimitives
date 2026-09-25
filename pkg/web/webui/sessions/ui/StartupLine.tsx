import { Loader2 } from "lucide-react";
import { useSessionSignals } from "../../chat/ui/sessionSignals";

// StartupLine is the shell's own answer to "why is nothing happening yet". The
// health watcher's session_startup frames — folded into signals.startup by the
// session socket the shell holds around both views — say what is in the way
// of the agent starting, in the words channelsd puts on the session's channel
// thread, and whether the wait has outlived the short startup grace. Chrome,
// not content: the transcript and the agent-defined page both get it, and it
// goes away on the frame that says the session started.
export function StartupLine(): JSX.Element | null {
  const { startup } = useSessionSignals();
  if (startup === null) return null;
  return (
    <div
      data-testid="session-shell-startup"
      role="status"
      className="flex items-center gap-2 border-b border-border bg-card/60 px-4 py-2 text-sm text-muted-foreground"
    >
      <Loader2 className="h-4 w-4 shrink-0 animate-spin" aria-hidden="true" />
      <span>
        {startup.short !== null ? (
          <>
            <span className="hidden sm:inline">{startup.text}</span>
            <span className="sm:hidden">{startup.short}</span>
          </>
        ) : (
          startup.text
        )}
        {startup.stillTrying && <span data-testid="session-shell-startup-still-trying"> — still trying</span>}
      </span>
    </div>
  );
}
