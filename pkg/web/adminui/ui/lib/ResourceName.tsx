// ResourceName renders a canonical "namespace/name" resource identifier for
// DISPLAY: the namespace segment is muted (it's context, not the identity) and
// the "default" namespace is hidden entirely (the ubiquitous common case —
// showing "default/" on every row is pure noise). The name is always shown.
// Callers pass either the combined `id` ("ns/name") or explicit `ns` + `name`.
//
// Only the display changes — callers that also link (EntityLink) keep the full
// "ns/name" as the route id; ResourceName is purely the label.

// splitResourceId splits a canonical "namespace/name" id. Namespaces and names
// never contain "/", so a single split on the first slash is exact; a bare id
// (no slash) is a name with no namespace.
export function splitResourceId(id: string): { ns: string; name: string } {
  const i = id.indexOf("/");
  return i < 0 ? { ns: "", name: id } : { ns: id.slice(0, i), name: id.slice(i + 1) };
}

// isHiddenNamespace reports whether a namespace should be elided from display:
// the empty namespace (cluster-scoped / bare id) has nothing to show, and the
// "default" namespace is the common case we hide to cut clutter.
export function isHiddenNamespace(ns: string): boolean {
  return ns === "" || ns === "default";
}

// resourceDisplayName is the plain-string display for contexts that cannot take
// JSX (title/aria attributes, non-styled text): the bare name when the namespace
// is hidden, else "namespace/name".
export function resourceDisplayName(id: string): string {
  const { ns, name } = splitResourceId(id);
  return isHiddenNamespace(ns) ? name : `${ns}/${name}`;
}

export interface ResourceNameProps {
  // Either supply the combined "ns/name" id …
  id?: string;
  // … or the two parts explicitly (where the caller already has them split).
  ns?: string;
  name?: string;
  // className styles the whole label (the name inherits it — e.g. the primary
  // link color); the namespace prefix always overrides to muted.
  className?: string;
}

export function ResourceName({ id, ns, name, className }: ResourceNameProps) {
  const parts = id != null ? splitResourceId(id) : { ns: ns ?? "", name: name ?? "" };
  return (
    <span className={className}>
      {!isHiddenNamespace(parts.ns) && <span className="text-muted-foreground">{`${parts.ns}/`}</span>}
      {parts.name}
    </span>
  );
}
