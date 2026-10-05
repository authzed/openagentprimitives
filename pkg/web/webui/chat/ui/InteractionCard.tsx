import { useState } from "react";
import {
  AlertTriangle,
  CheckCircle2,
  Clock,
  ExternalLink,
  Eye,
  FolderGit2,
  Github,
  KeyRound,
  Pencil,
  XCircle,
  type LucideIcon,
} from "lucide-react";
import { Button, cn } from "@ap/design";
import { CardExcerpt } from "./cardParts";
import { compactConsentReview, ConsentReview } from "./consentReview";
import type {
  InteractionAction,
  InteractionAppliedInner,
  InteractionField,
  InteractionItem,
  InteractionRequestInner,
} from "./types";

// outcomeIcon maps an InteractionAppliedInner.outcome (channelevents.Outcome*)
// to its resolved-state glyph, mirroring MessageList's planItemIcon. Unknown
// outcomes fall back to a neutral check — a resolved card always reads as
// "done", never as an error, unless the outcome says otherwise.
function outcomeIcon(outcome: string) {
  switch (outcome) {
    case "denied":
      return <XCircle className="h-3.5 w-3.5 shrink-0 text-destructive" aria-hidden="true" />;
    case "expired":
      return <Clock className="h-3.5 w-3.5 shrink-0 text-muted-foreground" aria-hidden="true" />;
    default: // "approved" | "resolved" | anything else
      return <CheckCircle2 className="h-3.5 w-3.5 shrink-0 text-success" aria-hidden="true" />;
  }
}

// IDENTITY_CHOICE_OUTCOME_LABELS maps identity_choice's 3-way ActionID
// (agent / userPassthrough / cancel) to the friendly text shown for a
// resolved prompt. The ActionID itself rides the wire verbatim in
// InteractionAppliedInner.outcomeText (channelsd's IdentityChoiceDecisionHandler
// sets it, and internal/cmd/runner's subscribeInteractionApplied reads it straight
// back out as the identity gate's decision action) — it is load-bearing and
// must never be replaced at the source, so this mapping is applied only at
// render time. Mirrors pkg/channels/channelinteractions/categories.IdentityChoiceOutcomeLabel
// (Slack's equivalent) — keep the label text in sync if either changes.
// outcomeText is also category-overloaded (credential_link uses it for a
// credential name), so callers must gate on category === "identity_choice"
// before consulting this map — see interactionOutcomeText below.
const IDENTITY_CHOICE_OUTCOME_LABELS: Record<string, string> = {
  agent: "Running as the agent",
  userPassthrough: "Running as you",
  cancel: "Cancelled",
};

// OUTCOME_HEADLINES is the human word for an InteractionAppliedInner.outcome
// (channelevents.Outcome*). Those constants are wire enum values, and several
// categories resolve carrying nothing else — both tool-approval decision
// handlers return an Outcome with no outcomeText — so without this, "approved"
// is what reaches the user. Mirrors channelevents.OutcomeHeadline, which serves
// Slack and the oap chat TUI; keep the words in sync if either changes.
const OUTCOME_HEADLINES: Record<string, string> = {
  approved: "Approved",
  denied: "Denied",
  expired: "Expired",
  resolved: "Resolved",
};

// interactionOutcomeText renders the resolved-state text for an applied
// interaction: identity_choice's raw action id swaps for its friendly label;
// every other category (or an unrecognized identity_choice action id) falls
// back to the raw outcomeText, then to the outcome's human headline. Mirrors
// pkg/channels/channelinteractions/categories.OutcomeLabel step for step, including its
// TrimSpace — outcomeText that is only whitespace is absent, not a label, and
// `||` alone would render it as a blank line.
//
// An outcome this build does not recognize passes through unchanged: a renderer
// must never be the reason a resolution goes silent.
function interactionOutcomeText(applied: InteractionAppliedInner): string {
  if (applied.category === "identity_choice") {
    const label = IDENTITY_CHOICE_OUTCOME_LABELS[applied.outcomeText ?? ""];
    if (label) return label;
  }
  const text = (applied.outcomeText ?? "").trim();
  if (text !== "") return text;
  return OUTCOME_HEADLINES[applied.outcome] ?? applied.outcome;
}

// actionButtonVariant maps InteractionAction.style (channelevents.ActionStyle
// — a rendering hint only) onto the shared Button's variant.
function actionButtonVariant(style: string | undefined): "default" | "destructive" | "secondary" {
  if (style === "primary") return "default";
  if (style === "danger") return "destructive";
  return "secondary";
}

// ActionButton renders one InteractionAction: a link/link_mint action as an
// anchor styled as a button (asChild — same shadcn pattern as everywhere
// else a link needs button chrome), a decision action as a real button that
// calls onDecision and disables itself once ANY action on this card has been
// clicked (pending), so a double-click can't fire two decisions.
export function ActionButton({
  action,
  pending,
  onClick,
}: {
  action: InteractionAction;
  pending: boolean;
  onClick: () => void;
}) {
  const variant = actionButtonVariant(action.style);
  if (action.kind === "link" || action.kind === "link_mint") {
    // A link_mint action's real URL only exists after resolution (it arrives
    // via InteractionAppliedInner.mintedUrl); a still-pending link_mint has
    // no url yet, so it isn't clickable. Neither category in this slice
    // (credential_link) uses link_mint — the shape is honored regardless.
    if (!action.url) return null;
    return (
      <Button asChild variant={variant} size="sm">
        <a href={action.url} target="_blank" rel="noopener noreferrer">
          {action.label}
          <ExternalLink className="h-3.5 w-3.5" aria-hidden="true" />
        </a>
      </Button>
    );
  }
  return (
    <Button variant={variant} size="sm" disabled={pending} onClick={onClick}>
      {action.label}
    </Button>
  );
}

// TIER_TONES maps the three blast-radius tones onto a glyph AND a design
// token, together — colour alone fails a monochrome display, a screenshot,
// and red-green colour deficiency, the same reason the external badge below
// repeats its warning as text. "muted" is not a tier (it marks supporting
// detail, not blast radius) and is handled separately below.
const TIER_TONES: Partial<
  Record<NonNullable<InteractionItem["tone"]>, { Icon: LucideIcon; textClass: string }>
> = {
  readonly: { Icon: Eye, textClass: "text-success" },
  readwrite: { Icon: Pencil, textClass: "text-primary" },
  external: { Icon: AlertTriangle, textClass: "text-warning" },
};

// RESOURCE_ICONS is the CLOSED, code-defined registry InteractionItem.icon
// may name — mirrors plangate's own knownIcons row for row. An icon name not
// in this map renders NOTHING: never a fallback image, never a guess (the
// same invariant DeriveLabel enforces for a display's derived label). Grows
// by row as new resource types need a mark.
const RESOURCE_ICONS: Record<string, LucideIcon> = {
  repository: FolderGit2,
  github: Github,
};

// FieldItems renders a structured field: one row per item, children indented
// under their parent.
//
// Category-agnostic on purpose, exactly like the rest of the card. Nothing here
// knows what a phase is — it lays out text/detail/tone/hint/icon/href, and the
// publisher decides what those mean. A second category with a list to show
// gets this rendering for free.
//
// `tone` is emphasis only. A row whose tone is dropped still reads correctly,
// because the row's own text says what it is; external reach additionally
// carries its warning in `detail`, so the meaning survives even where colour
// cannot (a screenshot, a copy-paste, a monochrome display).
//
// `icon` names a resource type's declared mark (RESOURCE_ICONS, a closed
// registry mirroring plangate's knownIcons); an unrecognized name renders no
// icon at all, never a guess.
//
// `href`, when present, makes `detail` a real link. The rendered anchor's
// text is `href` itself — not `detail`, not `text` — so text == href holds
// regardless of what the payload claims about the two matching.
//
// `hint` is the raw permission handle: supplementary text a human can pull up
// on demand (the `title` tooltip below) and NEVER rendered inline — a
// decision is made from `text`, not from a handle string.
//
// Depth 1 carries NO left indent. Its parent is always a bordered, tinted
// section box (isSection requires depth 0), so the box already says these
// lines belong to that phase; indenting inside it says the same thing twice
// and lands the icon column in the dead zone between the phase number and the
// phase label, aligned to neither. Deeper levels keep the indent — they have
// no box of their own to group them.
export function FieldItems({ items, depth = 0 }: { items: InteractionItem[]; depth?: number }) {
  // A top-level item that HAS children is a section — a plan-gate phase, with
  // its permissions and the resources it reaches beneath it. One WITHOUT
  // children is a statement about the card as a whole (the plan gate's coverage
  // line: does saying yes finish this, or is it the first of several prompts?).
  // Numbering only the sections keeps that distinction visible, and derives it
  // from the shape the publisher already sent rather than from a category this
  // component is not supposed to know about.
  let sectionNo = 0;
  return (
    <div className={cn("flex flex-col", depth === 0 ? "gap-1.5" : depth === 1 ? "gap-1" : "gap-0.5 pl-2")}>
      {items.map((it, i) => {
        const isSection = depth === 0 && !!it.items?.length;
        if (isSection) sectionNo += 1;
        const tier = it.tone ? TIER_TONES[it.tone] : undefined;
        // An unrecognized name looks up to undefined, which renders nothing
        // below — the same closed-registry contract the derived label
        // itself follows server-side.
        const ResourceIcon = it.icon ? RESOURCE_ICONS[it.icon] : undefined;
        return (
          <div
            key={`${it.text}:${i}`}
            className={cn(isSection && "rounded-md border border-border/60 bg-muted/30 px-3 py-2.5")}
          >
            <div className="flex flex-wrap items-baseline gap-x-1.5">
              {isSection && (
                <span className="rounded bg-state/15 px-1 py-px font-mono text-[10px] font-medium text-state">
                  {sectionNo}
                </span>
              )}
              {tier && (
                // The glyph half of the tier's double encoding — present
                // whenever the colour is, found by test as `tier-<tone>`.
                <tier.Icon
                  data-testid={`tier-${it.tone}`}
                  className={cn("h-3 w-3 shrink-0", tier.textClass)}
                  aria-hidden="true"
                />
              )}
              {ResourceIcon && (
                // A resource type's declared mark — found by test as
                // `resource-icon-<name>`, mirroring `tier-<tone>` above.
                <ResourceIcon
                  data-testid={`resource-icon-${it.icon}`}
                  className="h-3 w-3 shrink-0 text-muted-foreground"
                  aria-hidden="true"
                />
              )}
              <span
                title={it.hint || undefined}
                className={cn(
                  isSection && "font-medium",
                  // The dotted underline is the only visible sign that hovering
                  // reveals more — it must never be the handle text itself.
                  it.hint && "cursor-help underline decoration-dotted decoration-muted-foreground/60 underline-offset-2",
                  it.tone === "muted"
                    ? "text-muted-foreground"
                    : tier
                      ? cn("font-medium", tier.textClass)
                      : "text-card-foreground",
                )}
              >
                {it.text}
              </span>
              {it.detail && it.href ? (
                // text == href, enforced HERE rather than trusted from the
                // payload: the anchor's visible text is href itself, not
                // detail and not text — so no renderer bug can make the two
                // diverge, whatever the publisher sent. https is the
                // publisher's job to have already enforced (eligibleHref);
                // this only refuses to let rendering re-derive the text from
                // a second source.
                <a
                  href={it.href}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="break-all font-mono text-[11px] text-link underline decoration-link/40 underline-offset-2 hover:decoration-link"
                >
                  {it.href}
                </a>
              ) : (
                it.detail && (
                  <span
                    className={cn(
                      "break-all",
                      it.tone === "external"
                        ? "rounded bg-warning/15 px-1.5 py-px text-[10px] font-semibold uppercase tracking-wide text-warning ring-1 ring-inset ring-warning/30"
                        : // A resource identifier is technical text a human is
                          // asked to recognise character by character — the
                          // difference between two repositories can be one of
                          // them. Mono, and never truncated.
                          "font-mono text-[11px] text-muted-foreground",
                    )}
                  >
                    {it.detail}
                  </span>
                )
              )}
            </div>
            {it.items && it.items.length > 0 && (
              <div className={cn(depth === 0 && "mt-2")}>
                <FieldItems items={it.items} depth={depth + 1} />
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
}

// anyExternal reports whether any line on the card is marked as reach whose
// effects leave the session. Walks children too: the plan gate nests its
// permission lines one level under a phase, so a top-level-only check would
// miss every one of them.
function anyExternal(fields: InteractionField[] | undefined): boolean {
  const walk = (items: InteractionItem[] | undefined): boolean =>
    (items ?? []).some((it) => it.tone === "external" || walk(it.items));
  return (fields ?? []).some((f) => walk(f.items));
}

// InteractionCard renders one channelevents interaction prompt: the generic
// request/applied envelope that replaced the per-prompt-type kind families
// (tool approval, credential link, info leakage, identity choice, ...).
// Category-agnostic by design — the card renders Lead/Body/Fields/Excerpt/
// Actions the same way regardless of which category published them.
//
// SECURITY CONTRACT: `request.excerpt` is UNTRUSTED (see InteractionExcerpt
// in types.ts) — it renders inside a <pre> block as literal text, never
// interpreted as markup. Every other field is publisher-authored/trusted.
//
// `footer` is an opaque slot rendered between the fields and the actions. It
// keeps this component category-agnostic while letting a caller fold related
// content into the SAME card: the plan gate passes the plan's phase-grouped
// steps, so an approval and the plan it clears read as one thing instead of two
// cards describing the same work differently. Nothing here interprets it.
export function InteractionCard(props: {
  request: InteractionRequestInner;
  applied?: InteractionAppliedInner;
  onDecision: (requestRef: string, category: string, actionId: string) => void;
  footer?: React.ReactNode;
}) {
  const { request, applied, onDecision, footer } = props;
  const consentReview = compactConsentReview(request);
  const displayFields = consentReview?.parentFields ?? request.fields;
  // A card that asks about reach whose effects leave the session and cannot be
  // undone gets the strongest treatment on the surface. Derived from the tone
  // the publisher already set on the line, so this stays category-agnostic —
  // nothing here decides WHAT is external, only how loudly to say so.
  const hasExternal = anyExternal(request.fields);
  // pending latches the instant any decision action is clicked, disabling
  // every decision button on the card so a slow round-trip can't be double-
  // fired from a second click. Cleared implicitly once `applied` arrives
  // (the card re-renders resolved and no longer shows actions at all).
  const [pending, setPending] = useState(false);
  const resolved = !!applied;

  return (
    <div
      className={cn(
        "mx-auto flex w-full max-w-[85%] flex-col gap-2 rounded-lg border bg-card/60 px-3 py-2.5",
        // A decision is PENDING: the card carries an accent rail so it reads as
        // something asked of the reader rather than as another notice scrolling
        // past. Resolved, it drops back to the ordinary border — the question is
        // answered and no longer wants the eye.
        resolved
          ? "border-border"
          : hasExternal
            ? "border-l-2 border-border border-l-warning"
            : "border-l-2 border-border border-l-primary",
      )}
      data-testid="interaction-card"
      data-request-ref={request.requestRef}
      data-external={hasExternal ? "true" : undefined}
    >
      <div className="flex items-start gap-1.5">
        <span className="mt-0.5">
          {resolved ? (
            outcomeIcon(applied.outcome)
          ) : hasExternal ? (
            <AlertTriangle className="h-3.5 w-3.5 shrink-0 text-warning" aria-hidden="true" />
          ) : (
            <KeyRound className="h-3.5 w-3.5 shrink-0 text-primary" aria-hidden="true" />
          )}
        </span>
        {/* The lead is the question being asked. It was rendered at the same
            weight as every muted caption on the card, which is why an approval
            read as a status line; it is the one thing that must be legible at a
            glance. */}
        <span
          className={cn(
            "break-words text-sm font-semibold leading-snug",
            resolved ? "text-muted-foreground" : "text-card-foreground",
          )}
        >
          {request.lead}
        </span>
      </div>

      {request.body && <div className="text-xs leading-snug text-muted-foreground">{request.body}</div>}

      {footer}

      {displayFields && displayFields.length > 0 && (
        <div className="flex flex-col gap-2.5">
          {displayFields.map((f, i) => {
            // Structure wins when the publisher sent it. A plan-gate What is a
            // list of phases, each with its permissions and the resources it
            // reaches, and printing that as one newline-joined paragraph made
            // the browser — the surface with a design system and the most room
            // — the worst place to read an approval.
            if (f.items && f.items.length > 0) {
              const eyebrow = "text-[10px] font-medium uppercase tracking-wide text-muted-foreground";
              // Once the card is resolved it is a RECORD, not a decision
              // surface. The tree is kept in full — an approval that cannot be
              // re-read afterwards is not auditable — but folded away, because
              // a settled decision should not go on pushing the rest of the
              // transcript down the page.
              //
              // <details> rather than React state: the open/closed bit is
              // presentation the browser already owns, it works before hydration
              // and without JS, and it is keyboard- and screen-reader-accessible
              // for free. The count travels in the summary so a reader knows how
              // much is behind it without opening it. Deliberately NOT the word
              // "phases" — this component stays category-agnostic, and the
              // publisher's own label is the only domain word on the row.
              if (resolved) {
                return (
                  <details key={`${f.label}:${i}`} className="text-xs leading-snug">
                    <summary className={cn(eyebrow, "cursor-pointer hover:text-foreground")}>
                      {f.label}{" "}
                      <span className="font-normal normal-case tracking-normal text-muted-foreground/70">
                        ({f.items.length})
                      </span>
                    </summary>
                    <div className="mt-2">
                      <FieldItems items={f.items} />
                    </div>
                  </details>
                );
              }
              return (
                <div key={`${f.label}:${i}`} className="flex flex-col gap-1 text-xs leading-snug">
                  <div className={eyebrow}>{f.label}</div>
                  <FieldItems items={f.items} />
                </div>
              );
            }
            // A plan-gate card's What is multi-line by construction: one line
            // per permission in the phase's ceiling, then a line per resource
            // it asks to reach. HTML folds newlines into spaces, so an inline
            // span ran the whole ceiling and every resource together on one
            // line — and the resource list is the part the approver is
            // actually deciding on. Slack renders these as mrkdwn lines and
            // the TUI as plain text; without this webchat was the one surface
            // where the same approval read differently.
            //
            // Applied only when the value HAS newlines, so every single-line
            // field keeps the inline label/value row it has today.
            const multiline = f.value.includes("\n");
            return (
              // Keyed by label+index, not label alone: two fields can share a
              // label (e.g. repeated "Scope:" rows) and a bare-label key would
              // collide, causing React to conflate/misrender the rows.
              // Label ABOVE the value, not inline before it. These fields are
              // the card's supporting matter — who may approve, what one click
              // covers, the agent's own stated reason — and inline
              // `Label: long sentence` rows ran together into a paragraph the
              // eye could not enter. Stacked with an eyebrow, each one is
              // findable without being read in sequence.
              <div key={`${f.label}:${i}`} className="flex flex-col gap-0.5 text-xs leading-snug">
                <div className="text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
                  {f.label}
                </div>
                <span
                  className={cn(
                    "text-card-foreground",
                    multiline && "whitespace-pre-wrap break-words",
                  )}
                >
                  {f.value}
                </span>
              </div>
            );
          })}
        </div>
      )}

      {/* UNTRUSTED — rendered as literal text inside <pre><code>, never as
          markup. See the file-top SECURITY CONTRACT note and cardParts.tsx. */}
      {consentReview ? (
        <>
          <ConsentReview review={consentReview} />
          <details className="text-xs leading-snug">
            <summary className="cursor-pointer text-muted-foreground hover:text-foreground">
              View goal outcomes and evidence
            </summary>
            <CardExcerpt excerpt={request.excerpt} />
          </details>
        </>
      ) : <CardExcerpt excerpt={request.excerpt} />}

      {!!request.consents?.length && (
        <details className="text-xs leading-snug">
          <summary className="cursor-pointer text-muted-foreground hover:text-foreground">
            View exact requests included in this approval
          </summary>
          <pre className="mt-2 max-h-96 overflow-auto whitespace-pre-wrap break-words">
            {JSON.stringify(request.details, null, 2)}
          </pre>
        </details>
      )}

      {resolved ? (
        <div className={cn("text-xs", applied.outcome === "denied" ? "text-destructive" : "text-muted-foreground")}>
          {interactionOutcomeText(applied)}
        </div>
      ) : (
        request.actions &&
        request.actions.length > 0 && (
          // Separated from the content above, so the buttons read as the answer
          // to the card rather than as another row of it.
          <div className="mt-0.5 flex flex-wrap justify-end gap-1.5 border-t border-border/60 pt-2">
            {request.actions.map((a) => (
              <ActionButton
                key={a.id}
                action={a}
                pending={pending}
                onClick={() => {
                  setPending(true);
                  onDecision(request.requestRef, request.category, a.id);
                }}
              />
            ))}
          </div>
        )
      )}
    </div>
  );
}
