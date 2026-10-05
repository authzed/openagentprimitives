import { Clock } from "lucide-react";
import type { SessionOpening } from "./types";

// Both strings are producer-authored and may contain untrusted goal or trigger
// content. Render them literally, including the exact instruction whitespace.
export function OpeningCard({ opening }: { opening: SessionOpening }) {
  return (
    <div className="mx-auto w-full max-w-[85%] rounded-lg border border-border bg-card/60 px-3 py-2.5" data-testid="opening-card">
      <div className="flex items-start gap-2 text-xs text-card-foreground">
        <Clock className="mt-0.5 h-3.5 w-3.5 shrink-0 text-muted-foreground" aria-hidden="true" />
        <span className="break-words">{opening.summary}</span>
      </div>
      <details className="mt-2 text-xs">
        <summary className="cursor-pointer text-muted-foreground hover:text-card-foreground">View exact instructions</summary>
        <pre className="mt-2 max-h-96 overflow-auto whitespace-pre-wrap break-words rounded-md border border-border bg-background p-3 text-card-foreground">{opening.instructions}</pre>
      </details>
    </div>
  );
}
