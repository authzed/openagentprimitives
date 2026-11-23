import { CardExcerpt, CardFields } from "./cardParts";
import type { InteractionField, InteractionRequestInner } from "./types";

// Compact only the complete, recognized projection. Unrecognized cards keep
// their original display, and the signed request and exact disclosure stay intact.
export function compactConsentReview(request: InteractionRequestInner) {
  const children = request.consents;
  if (!children?.length) return null;
  const expanded = children.flatMap((child, i) => {
    const prefix = `Reminder ${i + 1}`;
    return [
      { label: prefix, value: child.lead },
      ...(child.body ? [{ label: `${prefix} · Execution`, value: child.body }] : []),
      ...(child.fields ?? []).map((field) => ({ ...field, label: `${prefix} · ${field.label}` })),
    ];
  });
  const fields = request.fields ?? [];
  if (fields.length < expanded.length ||
      JSON.stringify(fields.slice(-expanded.length)) !== JSON.stringify(expanded) ||
      children.some((child) => child.fields?.some((field) => field.items?.length))) return null;

  const equal = (a: InteractionField, b: InteractionField) => JSON.stringify(a) === JSON.stringify(b);
  const shared = (children[0].fields ?? []).filter((field) =>
    children.every((child) => child.fields?.some((other) => equal(field, other))));
  const sharedBody = children.every((child) => child.body === children[0].body) ? children[0].body : undefined;
  return {
    parentFields: fields.slice(0, fields.length - expanded.length),
    shared,
    sharedBody,
    reminders: children.map((child) => ({
      child,
      fields: (child.fields ?? []).filter((field) => !shared.some((other) => equal(field, other))),
    })),
  };
}

export function ConsentReview({ review }: { review: NonNullable<ReturnType<typeof compactConsentReview>> }) {
  return (
    <div className="flex flex-col gap-2" data-testid="compact-consent-review">
      {review.reminders.map(({ child, fields }, i) => (
        <section key={child.requestRef} aria-label={`Reminder ${i + 1}`} className="flex flex-col gap-1">
          {/* Goal text remains untrusted, literal, and visually fenced. */}
          <CardExcerpt excerpt={{ label: `Reminder ${i + 1}`, content: child.excerpt?.content.split("\n")[0] || child.lead }} />
          {child.body && child.body !== review.sharedBody && <p className="text-xs">{child.body}</p>}
          <CardFields fields={fields} />
        </section>
      ))}
      {review.sharedBody && <p className="text-xs text-muted-foreground">{review.sharedBody}</p>}
      <CardFields fields={review.shared} />
    </div>
  );
}
