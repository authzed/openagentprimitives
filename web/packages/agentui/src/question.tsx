// question.tsx renders ap:question — a control that asks the viewer
// something and sends the answer through the SAME route a Prompt action's
// filled sentence already travels: a message in the session's transcript,
// exactly what the viewer could have typed themselves. That is why this
// component carries no `action` prop and never touches useActions().invoke —
// there is nothing here to authorize beyond what the viewer could have
// typed, so QuestionCard's only seam into the page is useActions().answer.
//
// "Sent" is a TERMINAL state, not a waiting one: the agent's reply is a turn
// in the transcript and, if it composes one, a view update — holding this
// card on "waiting" would leave it stuck forever for a prose answer that
// never arrives back through this same seam. Whether a viewer still needs a
// cue is the PAGE's call, derived from the tree (see hooks.tsx's `waiting`
// and tree.ts's hooksContaining) — this component only ever reports its own
// idle/sending/sent/failed state, never "am I still on screen".
import * as React from "react";
import { Button, Card, CardContent, Label } from "@ap/design";
import { useActions } from "./actions";
import { useEnclosingHook, useHookState } from "./hooks";
import { p, s } from "./props";
import type { Node } from "./types";

interface Choice {
  label: string;
  value: string;
}

// asChoices guards `choices` the same way registry.tsx's requireArray guards
// ap:table/ap:steps' array props, except by falling back to "no options"
// rather than throwing: unlike those, a question with no valid choices still
// has somewhere sane to land — the prompt renders, nothing is selectable, and
// the submit button (disabled while empty) simply never has a value to send.
// A throw here would run inside this component's own body, past renderNode's
// try/catch (its doc comment explains why that catch cannot be a React error
// boundary), and take the whole page down for one malformed prop.
function asChoices(v: unknown): Choice[] {
  if (!Array.isArray(v)) return [];
  // Per ENTRY, the same way progress.tsx keeps only the strings out of its
  // arrays: a malformed member must cost that member, not the question. Both
  // fields are load-bearing — `value` is what gets SENT (and the radio group's
  // key), and `label` is rendered as a text child, where a non-string throws
  // inside React's own render, past renderNode's try/catch.
  return v.filter(
    (c): c is Choice =>
      !!c && typeof c.value === "string" && typeof c.label === "string",
  );
}

// QuestionCard is the registry's entry point, and it exists to give the card's
// state an IDENTITY that survives the way declarations are repainted.
//
// The agent asks one question at a time by replacing the node in a hook it
// already painted: the tree position does not move, only the props change.
// oap:generative renders its children by index and renderNode overwrites each
// element's key with that index, so React sees the same component at the same
// position and preserves the fiber — and with it "sent", a TERMINAL state. The
// next question would render as "Sent — waiting for the agent." with no form,
// and nothing on screen would ask again.
//
// The key on the BODY is what fixes it: it is nested inside this element, so
// renderNode's key override lands on the wrapper and leaves it alone. A
// different question is a different key, so React discards the answered card
// and mounts a fresh one; the SAME question re-rendered keeps its key, so a
// half-typed answer survives a repaint that changed nothing here.
export function QuestionCard({ n }: { n: Node }): JSX.Element {
  // The question's own content, not a counter or a render tick: what makes two
  // questions different is what they ASK, which is exactly the props the body
  // reads. Stringified because the value has to compare by content — `choices`
  // is a fresh array on every parse of the declaration.
  const identity = JSON.stringify([
    s(n, "prompt"),
    s(n, "kind"),
    p(n).choices ?? null,
  ]);
  return <QuestionCardBody key={identity} n={n} />;
}

function QuestionCardBody({ n }: { n: Node }): JSX.Element {
  const { answer, busy } = useActions();
  // The server-computed counterpart to local `sent`, below: a question whose
  // enclosing hook the server says the viewer already answered renders sent
  // on the FIRST paint, before this component's own `sent` state has ever
  // fired — the case a reload always is. hookName comes from context (see
  // hooks.tsx's GenerativeHook/useEnclosingHook), never a prop, because
  // nothing here is told which hook it is inside; a card outside any hook
  // reads null and can never be "answered".
  const hookName = useEnclosingHook();
  const { answered } = useHookState();
  const answeredHere = hookName !== null && answered.has(hookName);
  const prompt = s(n, "prompt");
  const kind = s(n, "kind") === "choice" ? "choice" : "text";
  const placeholder = s(n, "placeholder");
  const submitLabel = s(n, "submitLabel", "Answer");
  const choices = asChoices(p(n).choices);
  // Doubles as the radio group name (kind=choice, so two cards on one page
  // never share a native group and steal each other's selection) and as the
  // text field's id (kind=text, so the prompt Label can point `htmlFor` at
  // it) — the two uses are mutually exclusive per render, so one id serves
  // both without collision.
  const groupName = React.useId();

  const [value, setValue] = React.useState("");
  const [sending, setSending] = React.useState(false);
  const [sent, setSent] = React.useState(false);
  const [failed, setFailed] = React.useState(false);

  if (sent || answeredHere) {
    return <p data-testid="ap-question-sent">Sent — waiting for the agent.</p>;
  }

  // busy: the page says the agent is working (ActionsContextValue.busy) — an
  // answer typed now would land mid-turn, so the card waits with the rest.
  const disabled = value.trim() === "" || sending || Boolean(busy);

  function handleSubmit(e: React.FormEvent<HTMLFormElement>) {
    e.preventDefault();
    if (disabled) return;
    setSending(true);
    setFailed(false);
    answer(value)
      .then(() => setSent(true))
      .catch((err: unknown) => {
        // The alert is the user-visible half of the no-silent-errors
        // contract; this is the other half — a log line naming what failed,
        // for whoever is debugging why a viewer's answer never arrived. Same
        // posture as renderNode's own console.error before its fail-visible
        // card.
        console.error("agentui: ap:question answer failed", err);
        setFailed(true);
      })
      .finally(() => setSending(false));
  }

  return (
    <Card>
      <CardContent className="pt-6">
        <form
          data-testid="ap-question"
          onSubmit={handleSubmit}
          className="flex flex-col gap-3"
        >
          {/* htmlFor only for kind=text, where the textarea below is the ONE
              control the prompt labels (the house pattern ActionForm's Label
              uses). A kind=choice prompt has no single control to point at —
              each option is already its own accessible label via its own
              wrapping Label below — so it renders unassociated there. */}
          <Label
            htmlFor={kind === "text" ? groupName : undefined}
            className="font-normal text-foreground"
          >
            {prompt}
          </Label>
          {kind === "choice" ? (
            <div className="flex flex-col gap-2">
              {choices.map((c) => (
                <Label
                  key={c.value}
                  className="flex items-center gap-2 font-normal"
                >
                  <input
                    type="radio"
                    name={groupName}
                    value={c.value}
                    checked={value === c.value}
                    disabled={sending || Boolean(busy)}
                    onChange={() => setValue(c.value)}
                    className="h-4 w-4 border-input text-primary focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50"
                  />
                  {c.label}
                </Label>
              ))}
            </div>
          ) : (
            <textarea
              id={groupName}
              placeholder={placeholder}
              value={value}
              disabled={sending || Boolean(busy)}
              onChange={(e) => setValue(e.target.value)}
              className="flex min-h-16 w-full rounded-md border border-input bg-transparent px-3 py-2 text-sm shadow-sm placeholder:text-muted-foreground disabled:cursor-not-allowed disabled:opacity-50"
            />
          )}
          <Button type="submit" disabled={disabled} className="self-start">
            {submitLabel}
          </Button>
          {failed && (
            <p role="alert" className="text-xs text-destructive">
              Could not send your answer. Try again.
            </p>
          )}
        </form>
      </CardContent>
    </Card>
  );
}
