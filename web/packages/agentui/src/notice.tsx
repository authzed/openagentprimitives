// notice.tsx renders ap:notice — the agent telling the person something while
// it waits for an EVENT rather than an answer. It is ap:question's sibling for
// the case where no words are wanted back: the view keeps its reply modal
// closed while one is on the page (AgentUIView derives that from the tree),
// and the card's buttons fire DECLARED actions through the same seam
// ap:button uses (useActions().invoke), so nothing here names a tool.
//
// Like question.tsx, this file must NOT import registry.tsx: registry.tsx is
// what imports it to register the renderer.
//
// A click is terminal for the buttons, not for the card: the agent replaces
// the notice when it has something new to say, and the sent line under the
// buttons tells the person their click landed until then.
//
// Dismissal is the person's, per browser: keyed on the notice's own content,
// so the same notice stays hidden across a repaint that changed nothing and a
// different one (any prop changed) shows again. A store that cannot be read
// or written never hides a notice — fail visible.
import * as React from "react";
import { X } from "lucide-react";
import { Button, Card, CardContent, CardHeader, CardTitle, Markdown } from "@ap/design";
import { cn } from "@ap/design/lib/utils";
import { actionCaption, isActionPending, useActions, type ActionState } from "./actions";
import { p, s } from "./props";
import type { Node } from "./types";

interface NoticeButton {
  label: string;
  action: string;
}

const TONES = ["info", "success", "warning"] as const;
type Tone = (typeof TONES)[number];

// Each tone is a border and a faint wash of one design-system token, the way
// ap:alert's warning rides on --warning: no third colour system, and every
// tone reads as distinct in both themes.
const TONE_CLASS: Record<Tone, string> = {
  info: "border-state/40 bg-state/[0.06]",
  success: "border-success/50 bg-success/[0.08]",
  warning: "border-warning/50 bg-warning/[0.08]",
};

// MAX_BUTTONS mirrors uicomponents.MaxNoticeButtons. The validator already
// refuses more; this keeps a stale bundle from drawing a row of ten.
const MAX_BUTTONS = 4;

// asButtons guards `buttons` per ENTRY, like question.tsx's asChoices: a
// malformed member costs that member, never the notice. A throw here would
// run inside this component's own body, past renderNode's try/catch.
function asButtons(v: unknown): NoticeButton[] {
  if (!Array.isArray(v)) return [];
  return v
    .filter((b): b is NoticeButton => !!b && typeof b.label === "string" && typeof b.action === "string" && b.action !== "")
    .slice(0, MAX_BUTTONS);
}

function toneOf(n: Node): Tone {
  const t = s(n, "tone");
  return (TONES as readonly string[]).includes(t) ? (t as Tone) : "info";
}

// noticeIdentity is what makes two notices the same notice — exactly the
// props the card reads, stringified so it compares by content. It is both the
// React key that remounts the card for a different notice (question.tsx's
// argument, verbatim) and the dismissal key.
export function noticeIdentity(n: Node): string {
  return JSON.stringify([s(n, "title"), s(n, "body"), toneOf(n), asButtons(p(n).buttons)]);
}

const DISMISSED_PREFIX = "agentui:notice:dismissed:";

function readDismissed(identity: string): boolean {
  try {
    return window.localStorage.getItem(DISMISSED_PREFIX + identity) === "1";
  } catch (err) {
    // Logged, not swallowed: an unreadable store shows the notice, and the
    // line says why a person keeps seeing one they dismissed.
    console.error("agentui: could not read dismissed notices", err);
    return false;
  }
}

function writeDismissed(identity: string): void {
  try {
    window.localStorage.setItem(DISMISSED_PREFIX + identity, "1");
  } catch (err) {
    console.error("agentui: could not remember a dismissed notice", err);
  }
}

export function NoticeCard({ n }: { n: Node }): JSX.Element {
  const identity = noticeIdentity(n);
  return <NoticeCardBody key={identity} n={n} identity={identity} />;
}

function NoticeCardBody({ n, identity }: { n: Node; identity: string }): JSX.Element | null {
  const { states, invoke, busy } = useActions();
  const [dismissed, setDismissed] = React.useState(() => readDismissed(identity));
  // The action the person clicked, if any. Terminal for the buttons: a
  // second click would be a second message or a second tool call.
  const [sent, setSent] = React.useState<string | null>(null);
  if (dismissed) return null;

  const tone = toneOf(n);
  const title = s(n, "title");
  const buttons = asButtons(p(n).buttons);
  const anyPending = buttons.some((b) => isActionPending((states[b.action] ?? { phase: "idle" }).phase));
  const disabled = sent !== null || anyPending || Boolean(busy);
  const sentState: ActionState | null = sent !== null ? (states[sent] ?? { phase: "submitted" }) : null;

  return (
    <Card data-testid="ap-notice" data-tone={tone} className={cn("relative", TONE_CLASS[tone])}>
      <button
        type="button"
        aria-label="Dismiss"
        className="absolute right-2 top-2 rounded p-1 text-muted-foreground hover:text-foreground focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
        onClick={() => {
          writeDismissed(identity);
          setDismissed(true);
        }}
      >
        <X className="h-4 w-4" aria-hidden="true" />
      </button>
      {title && (
        <CardHeader className="pb-2 pr-8">
          <CardTitle className="text-base">{title}</CardTitle>
        </CardHeader>
      )}
      <CardContent className={cn("flex flex-col gap-3 pr-8", !title && "pt-6")}>
        <Markdown>{s(n, "body")}</Markdown>
        {buttons.length > 0 && (
          <div className="flex flex-wrap gap-2">
            {buttons.map((b, i) => (
              <Button
                key={`${b.action}:${i}`}
                type="button"
                variant={i === 0 ? "default" : "secondary"}
                disabled={disabled}
                onClick={() => {
                  setSent(b.action);
                  invoke(b.action, undefined);
                }}
              >
                {b.label}
              </Button>
            ))}
          </div>
        )}
        {sentState !== null && (
          <p data-testid="ap-notice-sent" className="text-xs text-muted-foreground">
            {actionCaption(sentState) ?? "Sent."}
          </p>
        )}
      </CardContent>
    </Card>
  );
}
