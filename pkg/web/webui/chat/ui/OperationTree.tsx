import { cn } from "@ap/design";
import type { OperationActivityCallNode, OperationActivityNode } from "./types";
import { formatElapsed } from "./format";

// CallLine renders one tool call under an operation: its reason + elapsed
// time. The active leaf pulses (mirroring CaptionLine's live-caption pulse in
// MessageList.tsx); a finished-but-still-listed call grays out.
function CallLine({ call }: { call: OperationActivityCallNode }) {
  return (
    <div
      className={cn(
        "flex items-center gap-1.5 pl-4 text-[11px]",
        call.active ? "text-muted-foreground animate-pulse" : "text-muted-foreground/60",
      )}
    >
      <span className="truncate">{call.reason}</span>
      <span className="shrink-0">· {formatElapsed(call.elapsedSeconds)}</span>
    </div>
  );
}

// OperationLine renders one operation node: its description + elapsed time,
// with its calls nested beneath.
function OperationLine({ op }: { op: OperationActivityNode }) {
  return (
    <div className="flex flex-col gap-0.5">
      <div
        className={cn(
          "flex items-center gap-1.5 text-[11px]",
          op.active ? "text-muted-foreground animate-pulse" : "text-muted-foreground/60",
        )}
      >
        <span className="truncate">{op.description}</span>
        <span className="shrink-0">· {formatElapsed(op.elapsedSeconds)}</span>
      </div>
      {(op.calls ?? []).map((c, i) => (
        <CallLine key={i} call={c} />
      ))}
    </div>
  );
}

// OperationTree renders the runner's live operation-activity tree: each active
// operation with its description + elapsed time, and beneath it the tool calls
// it made (reason + elapsed). Purely presentational — ChatView owns the
// operation_activity frame handling and clear lifecycle (see its `opTree`
// state); this component only renders whatever nodes it's given. Renders
// nothing when there are no nodes, so it never leaves an empty shell behind in
// the live region.
export function OperationTree({ nodes }: { nodes: OperationActivityNode[] }) {
  if (nodes.length === 0) return null;
  return (
    <div className="flex flex-col gap-1 items-start" data-testid="operation-tree">
      {nodes.map((op) => (
        <OperationLine key={op.id} op={op} />
      ))}
    </div>
  );
}
