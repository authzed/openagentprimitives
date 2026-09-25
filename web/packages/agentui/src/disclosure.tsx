// disclosure.tsx is the ONE folding container. ap:collapsible renders it with
// the author's `collapsed`; a hook bound to a finished timeline step renders it
// with the page's verdict (hooks.tsx). Both must behave the same way, so both
// use this.
//
// The person's toggle is local state that wins over `collapsed` until the
// prop CHANGES, at which point the new value applies again — a fold the page
// closed because a phase ended, and the person reopened, stays open until the
// page has a new reason to say otherwise.
//
// A native <details>, controlled: the summary's click is intercepted so the
// open state has exactly one owner (React), which is also what makes the
// behaviour identical under jsdom and in a browser.
import * as React from "react";
import { ChevronRight } from "lucide-react";
import { cn } from "@ap/design/lib/utils";

export function Disclosure({
  title,
  collapsed,
  testId,
  children,
}: {
  title: string;
  collapsed: boolean;
  testId: string;
  children?: React.ReactNode;
}): React.ReactElement {
  const [open, setOpen] = React.useState(!collapsed);
  React.useEffect(() => {
    setOpen(!collapsed);
  }, [collapsed]);
  return (
    <details data-testid={testId} open={open} className="w-full">
      <summary
        className="flex cursor-pointer select-none items-center gap-2 px-1 py-2 text-sm font-medium [&::-webkit-details-marker]:hidden"
        onClick={(e) => {
          e.preventDefault();
          setOpen((o) => !o);
        }}
      >
        <ChevronRight
          className={cn(
            "h-4 w-4 shrink-0 transition-transform",
            open && "rotate-90",
          )}
          aria-hidden="true"
        />
        {title}
      </summary>
      <div className="px-1 pb-2">{children}</div>
    </details>
  );
}
