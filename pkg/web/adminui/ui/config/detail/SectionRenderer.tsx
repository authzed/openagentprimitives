import type { ReactNode } from "react";
import { Alert, AlertDescription } from "@ap/design";
import { EntityLink } from "../../lib/EntityLink";
import { KindBadge } from "../../lib/KindBadge";
import { isEntityKind } from "../../lib/router";
import { httpUrl } from "../../lib/safeUrl";
import { CopyCmd } from "../../lib/CopyCmd";
import type { Badge as BadgeT, DetailField, DetailListItem, Section } from "../../lib/api";
import { Field } from "./shared";

// COMMAND_VALUE_PREFIXES flags a field VALUE (not label) as a copy-pasteable
// shell command rather than plain text — e.g. the OAP Bundle section's "Pull
// command" field (`oap agent pull ghcr.io/acme/support-bot@sha256:… -o
// support-bot.oap`). Detected on the value, not the label, so this is a
// frontend-only affordance: no backend contract change is needed to flag a
// field copyable (see AccessView/SettingsView/ConfigDetail, which instead pass
// a whole-row manageCmd/grantCmd string straight to CopyCmd — those aren't
// generic "fields" Section rows, so they don't go through this detector).
const COMMAND_VALUE_PREFIXES: readonly string[] = ["oap agent pull "];

function isCommandValue(value: string): boolean {
  return COMMAND_VALUE_PREFIXES.some((p) => value.startsWith(p));
}

// KIND_FIELD_LABELS are the "fields" labels whose value is a kind token — a
// channel/provider transport kind or a credential kind/type. A field with one of
// these labels renders its value as a color-coded KindBadge (matching the kind
// coloring in the config tables and the live views) instead of plain text.
// Matched case-insensitively; kept an explicit, readable set rather than a
// substring heuristic so an unrelated "…kind of…" label never colorizes.
const KIND_FIELD_LABELS: ReadonlySet<string> = new Set([
  "kind",
  "channel kind",
  "credential kind",
  "credential type",
]);

function isKindLabel(label: string): boolean {
  return KIND_FIELD_LABELS.has(label.trim().toLowerCase());
}

// SectionRenderer renders one ResourceDetail Section by its kind:
//   - "fields": a label/value list; a field with a link becomes an EntityLink.
//   - "text":   a preformatted block (a system prompt, a SKILL.md body).
//   - "list":   titled rows with an optional subtitle, entity link, and badges.
// It is the shared per-tab body for every config detail page.
export function SectionRenderer({ section }: { section: Section }) {
  switch (section.kind) {
    case "text":
      return <TextSection text={section.text ?? ""} />;
    case "list":
      return <ListSection items={section.items ?? []} notice={section.text} />;
    case "fields":
    default:
      return <FieldsSection fields={section.fields ?? []} />;
  }
}

// LinkedValue renders `children` as an EntityLink when the entity slug is a known
// EntityKind; an unrecognized slug degrades to plain text (never a dead route).
function LinkedValue({ entity, id, children }: { entity: string; id: string; children: ReactNode }) {
  if (!isEntityKind(entity)) return <>{children}</>;
  return (
    <EntityLink entity={entity} id={id} className="font-mono text-xs text-link hover:underline">
      {children}
    </EntityLink>
  );
}

// ExternalAnchor renders `children` as a scheme-guarded external link (never a
// javascript:/data: href) opening in a new tab. Shared by the explicit-href and
// the auto-linked-value paths.
function ExternalAnchor({ href, children }: { href: string; children: ReactNode }) {
  return (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer nofollow"
      className="break-words font-mono text-xs text-link hover:underline"
    >
      {children}
    </a>
  );
}

// FieldValue renders one field's value. An admin-entity `link` wins (navigates
// within the console). An explicit `href` renders value as the TEXT of an
// external anchor (a Commit's short SHA linked to the full GitHub URL).
// Otherwise, an http(s) value (guarded by httpUrl — never a javascript:/data:
// href) becomes an external anchor so a bare repo/source URL is clickable
// instead of inert plain text. Everything else stays plain text.
function FieldValue({ field }: { field: DetailField }) {
  if (field.link) {
    return (
      <LinkedValue entity={field.link.entity} id={field.link.id}>
        {field.value}
      </LinkedValue>
    );
  }
  if (field.href) {
    const href = httpUrl(field.href);
    if (href) return <ExternalAnchor href={href}>{field.value}</ExternalAnchor>;
  }
  if (field.value && isKindLabel(field.label)) {
    return <KindBadge kind={field.value} />;
  }
  if (isCommandValue(field.value)) {
    return <CopyCmd text={field.value} />;
  }
  const url = httpUrl(field.value);
  if (url) return <ExternalAnchor href={url}>{field.value}</ExternalAnchor>;
  return <span className="break-words">{field.value}</span>;
}

// FieldsSection renders label/value rows; a field with a link navigates to
// another admin entity's detail page (or opens an external URL).
function FieldsSection({ fields }: { fields: DetailField[] }) {
  if (fields.length === 0) return <Empty />;
  return (
    <div className="flex flex-col gap-4">
      {fields.map((f, i) => (
        <Field key={`${f.label}-${i}`} label={f.label}>
          <FieldValue field={f} />
        </Field>
      ))}
    </div>
  );
}

// TextSection renders a free-form block verbatim (prompts, SKILL.md) as
// wrap-preserving monospace so whitespace/markdown structure survives.
function TextSection({ text }: { text: string }) {
  if (text === "") return <Empty />;
  return (
    <pre className="whitespace-pre-wrap break-words rounded-md border border-border bg-card p-4 font-mono text-xs text-foreground">
      {text}
    </pre>
  );
}

// ListSection renders titled rows (a tool, a credential, a subcommand). Each
// badge value renders as a KindBadge — a stable per-value color dot + label
// (shared with the config tables and live views) so the same kind/type reads the
// same color across the whole console.
//
// notice (a list Section's `text`) is a warning about the LIST ITSELF, rendered
// above the rows: a backend that could read only part of a list says so here,
// so a short list is never presented as a complete one. It renders even when
// there are no rows at all — "nothing could be read" and "there is nothing"
// are different answers, and only the notice tells them apart.
function ListSection({ items, notice }: { items: DetailListItem[]; notice?: string }) {
  const warning = notice ? (
    <Alert variant="destructive" className="mb-3">
      <AlertDescription>{notice}</AlertDescription>
    </Alert>
  ) : null;
  if (items.length === 0) {
    return (
      <>
        {warning}
        {!notice && <Empty />}
      </>
    );
  }
  return (
    <>
      {warning}
      <ul className="flex flex-col gap-2">
        {items.map((it, i) => (
          <li key={`${it.title}-${i}`} className="rounded-md border border-border bg-card p-3">
            <div className="flex flex-wrap items-center gap-2">
              <ListItemTitle item={it} />
              <BadgeChips badges={it.badges} />
            </div>
            {it.subtitle && <p className="mt-1 text-[11px] text-muted-foreground">{it.subtitle}</p>}
          </li>
        ))}
      </ul>
    </>
  );
}

// ListItemTitle renders one row's title, mirroring FieldValue's precedence so
// a row and a field linked the same way behave the same way: an admin-entity
// `link` wins (navigates within the console), then an explicit `href` makes the
// title the anchor text of an external link (a synced repository's
// `demo-org/widgets` pointing at its full URL), and otherwise the title is
// plain text. A title is NEVER auto-linked from its own text the way a field
// value is — a list title is a name, not a URL.
function ListItemTitle({ item }: { item: DetailListItem }) {
  if (item.link) {
    return (
      <LinkedValue entity={item.link.entity} id={item.link.id}>
        {item.title}
      </LinkedValue>
    );
  }
  if (item.href) {
    const href = httpUrl(item.href);
    if (href) return <ExternalAnchor href={href}>{item.title}</ExternalAnchor>;
  }
  return <span className="font-mono text-xs text-foreground">{item.title}</span>;
}

// BadgeChips renders each badge value as a stable color-coded KindBadge.
function BadgeChips({ badges }: { badges?: BadgeT[] }) {
  const list = badges ?? [];
  if (list.length === 0) return null;
  return (
    <span className="flex flex-wrap gap-1">
      {list.map((b, i) => (
        <KindBadge key={`${b.key}-${b.value}-${i}`} kind={b.value} />
      ))}
    </span>
  );
}

function Empty() {
  return <p className="text-sm text-muted-foreground">Nothing here.</p>;
}
