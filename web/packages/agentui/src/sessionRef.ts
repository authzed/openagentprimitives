// sessionRef.ts holds the one validator every renderer runs on a declared
// session reference before it becomes part of an iframe src — ap:session_view
// (registry.tsx) and ap:chat (chat.tsx) both call it and differ only in the
// prefix they put in front of the encoded segments.
//
// "ns/name" is exactly two non-empty, non-traversal segments — this IS the
// whole guard: more than two segments rejects extra-path injection and
// absolute URLs (their "//" splits into extra empty segments); "." and ".."
// reject traversal. Each caller's encodeURIComponent then makes every other
// character safe inside its own path segment, so do NOT add a character
// allowlist here — that would wrongly reject a valid hyphenated name
// ("weather-ai") or a space the encode tests expect.
//
// The segments come back unencoded: encoding belongs to the caller, which is
// the only place that knows the path it is building them into.
export function sessionRefSegments(ref: unknown): [string, string] | null {
  if (typeof ref !== "string") return null;
  const segs = ref.split("/");
  if (segs.length !== 2) return null;
  for (const seg of segs) {
    if (seg === "" || seg === "." || seg === "..") return null;
  }
  return [segs[0], segs[1]];
}
