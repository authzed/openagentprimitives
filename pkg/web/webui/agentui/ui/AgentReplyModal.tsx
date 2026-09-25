import * as React from "react";
import {
  Button,
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  Markdown,
} from "@ap/design";

// AgentReplyModal is the FLOOR beneath ap:question — what a viewer gets when
// the agent asked for something in prose and the page has no card asking it.
//
// The view decides when that is, from the declared tree: a page that holds an
// ap:question anywhere already asks, and this never opens beside it. So this is
// reached only for an agent that answered in words and stopped, which is the
// case that was otherwise silent — the turn ends waiting on a reply, and a
// viewer looking at the agent's page has nowhere to type one.
//
// The reply travels the transcript route, exactly as a typed message does. That
// is the whole safety argument: the text is something the viewer could have
// typed themselves, it lands where they can see it, and composing it from here
// grants nobody anything (see chat/ui/sessionMessage.ts).
//
// variant distinguishes the two reasons this floor opens. "asked" is the
// original case: the agent ended its turn WAITING for an answer
// (signals.awaitingReply), and the copy says so. "paused" is the runner's
// idle exit (signals.pausedIdle) — the agent replied and stopped without
// asking anything at all. Nobody is owed an answer there, but the turn still
// ended with nothing rendered on this view, which looks exactly like a dead
// page to someone who cannot see the transcript. Same body, same seams
// (onReply/onDismiss, the replySeq-keyed draft reset) — only the copy differs.
export function AgentReplyModal({
  open,
  variant,
  text,
  replySeq,
  onReply,
  onDismiss,
}: {
  open: boolean;
  variant: "asked" | "paused";
  text: string;
  // replySeq identifies WHICH reply this modal is showing — the session's
  // running count of agent replies, from the signals fold. It is the draft's
  // lifetime: see the reset effect below.
  replySeq: number;
  onReply: (text: string) => Promise<void>;
  onDismiss: () => void;
}): JSX.Element {
  const [value, setValue] = React.useState("");
  const [sending, setSending] = React.useState(false);
  const [failed, setFailed] = React.useState(false);

  // A later question must not arrive pre-filled with the answer to the earlier
  // one — and the draft must survive everything that is not a later question.
  // Keyed on the reply's identity rather than on `open`, because `open` flips
  // for reasons that have nothing to do with the ask: switching to the
  // transcript tab and back closes and reopens this modal over the SAME
  // question, and wiping a half-typed answer there is losing the viewer's work.
  React.useEffect(() => {
    setValue("");
    setFailed(false);
  }, [replySeq]);

  const disabled = value.trim() === "" || sending;

  function handleSubmit(e: React.FormEvent<HTMLFormElement>) {
    e.preventDefault();
    if (disabled) return;
    setSending(true);
    setFailed(false);
    onReply(value)
      .catch((err: unknown) => {
        // Both halves of the no-silent-errors contract: the alert below is what
        // the viewer sees, and this is what whoever is debugging a reply that
        // never arrived can grep for. A send that failed must never look like
        // one that landed — the dialog stays open holding the typed text, so
        // retrying costs nothing.
        console.error("agentui: reply modal send failed", err);
        setFailed(true);
      })
      .finally(() => setSending(false));
  }

  return (
    // Radix reports Escape and its own close control through onOpenChange(false).
    // Both mean the same thing as Later — "not now" — and routing them anywhere
    // else would leave the modal reopening on the next render for a question the
    // viewer already waved off.
    <Dialog open={open} onOpenChange={(next) => { if (!next) onDismiss(); }}>
      <DialogContent data-testid="agent-ui-reply-modal" data-variant={variant}>
        <DialogHeader>
          {variant === "paused" ? (
            <>
              <DialogTitle>The agent paused</DialogTitle>
              <DialogDescription>It replied and stopped without asking anything. Send a message to continue.</DialogDescription>
            </>
          ) : (
            <>
              <DialogTitle>The agent is waiting for your reply</DialogTitle>
              <DialogDescription>Your reply goes to this session&apos;s conversation, just as if you had typed it there.</DialogDescription>
            </>
          )}
        </DialogHeader>
        {/* The agent's own words, through the SAME renderer the transcript
            uses: identical bytes must read identically on both surfaces, and
            Markdown is what carries the guarantee that markup in agent text is
            escaped rather than executed. */}
        <div className="max-h-48 overflow-y-auto text-sm">
          <Markdown>{text}</Markdown>
        </div>
        <form onSubmit={handleSubmit} className="flex flex-col gap-3">
          <textarea
            aria-label="Your reply"
            value={value}
            disabled={sending}
            onChange={(e) => setValue(e.target.value)}
            placeholder={variant === "paused" ? "Tell it how to continue…" : undefined}
            className="flex min-h-20 w-full rounded-md border border-input bg-transparent px-3 py-2 text-sm shadow-sm placeholder:text-muted-foreground disabled:cursor-not-allowed disabled:opacity-50"
          />
          {failed && (
            <p role="alert" className="text-xs text-destructive">
              Could not send your reply. Try again.
            </p>
          )}
          <DialogFooter>
            {/* Later is a plain button, not the dialog's close control, because
                dismissing is a decision the view records (it remembers WHICH
                reply was waved off) and not merely a window closing. */}
            <Button type="button" variant="ghost" onClick={onDismiss} disabled={sending}>
              Later
            </Button>
            <Button type="submit" disabled={disabled}>
              {sending ? "Sending…" : variant === "paused" ? "Continue" : "Reply"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
