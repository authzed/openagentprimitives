// progress.tsx renders ap:progress — agent-authored status: what it is doing
// now, what it has finished, what comes next. It is read-only: unlike
// question.tsx it never calls useActions(), because there is nothing here
// for a viewer to act on, only something for them to watch.
import { Card } from "@ap/design";
import { p, s } from "./props";
import type { Node } from "./types";

const strings = (v: unknown): string[] => (Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : []);

function ProgressItem({ label, state }: { label: string; state: "done" | "next" }) {
  return (
    <li data-state={state} className="flex items-center gap-2 text-sm">
      {/* The checkmark is its OWN element, aria-hidden and outside the label's
          text node, so a query for the label text alone (getByText) matches
          exactly the label and nothing else. */}
      {state === "done" && (
        <span aria-hidden="true" className="text-[hsl(var(--success))]">
          ✓
        </span>
      )}
      <span className={state === "done" ? "text-muted-foreground line-through" : undefined}>{label}</span>
    </li>
  );
}

export function ProgressCard({ n }: { n: Node }): JSX.Element {
  const now = s(n, "now");
  const done = strings(p(n).done);
  const next = strings(p(n).next);
  const step = s(n, "step");
  return (
    <Card>
      <section data-testid="ap-progress" className="flex flex-col gap-3 p-6">
        <p data-testid="ap-progress-now" className="text-sm font-medium">
          {now}
        </p>
        {(done.length > 0 || next.length > 0) && (
          <ul className="flex flex-col gap-1.5">
            {done.map((label, i) => (
              <ProgressItem key={`done-${i}`} label={label} state="done" />
            ))}
            {next.map((label, i) => (
              <ProgressItem key={`next-${i}`} label={label} state="next" />
            ))}
          </ul>
        )}
        {step && <p className="text-xs text-muted-foreground">{step}</p>}
      </section>
    </Card>
  );
}
