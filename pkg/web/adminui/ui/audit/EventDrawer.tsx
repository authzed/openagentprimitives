import { Sheet, SheetContent, SheetHeader, SheetTitle } from "@ap/design";
import type { AuditEvent } from "../lib/api";

export function EventDrawer({ event, onClose }: { event: AuditEvent; onClose: () => void }) {
  return (
    <Sheet open onOpenChange={(open) => !open && onClose()}>
      <SheetContent side="right" className="w-full overflow-y-auto sm:max-w-xl">
        <SheetHeader>
          <SheetTitle className="text-sm">{event.summary}</SheetTitle>
        </SheetHeader>
        <dl className="mt-4 space-y-1 font-mono text-xs">
          <div><dt className="inline text-muted-foreground">when:</dt> <dd className="inline">{event.time}</dd></div>
          <div><dt className="inline text-muted-foreground">session:</dt> <dd className="inline">{event.sessionNamespace}/{event.sessionName}</dd></div>
          <div><dt className="inline text-muted-foreground">agent:</dt> <dd className="inline">{event.agentClass}</dd></div>
          <div><dt className="inline text-muted-foreground">actor:</dt> <dd className="inline">{event.actor}</dd></div>
          <div><dt className="inline text-muted-foreground">tool:</dt> <dd className="inline">{event.tool}</dd></div>
          <div><dt className="inline text-muted-foreground">entry:</dt> <dd className="inline">{event.entryId}</dd></div>
        </dl>
        <h3 className="mt-4 text-xs font-semibold uppercase text-muted-foreground">Raw</h3>
        <pre className="mt-1 overflow-x-auto rounded bg-card p-3 font-mono text-xs">
          {JSON.stringify(event.raw ?? {}, null, 2)}
        </pre>
      </SheetContent>
    </Sheet>
  );
}
