// httpUrl returns `s` unchanged only when it parses as an absolute URL whose
// scheme is http: or https: — the only schemes safe to render as a clickable
// anchor. Everything else (javascript:, data:, blob:, relative garbage) returns
// null so callers fall back to plain text.
//
// This is an XSS guard: React does NOT scrub javascript:/data: hrefs, so a
// free-form, lower-trust upstream URL (e.g. SkillSource.spec.repoURL, authored
// outside the platform admin's trust boundary) rendered as `<a href={repo}>`
// would be a clickable script in the admin's own session. Gate every such
// anchor on httpUrl(...) != null.
export function httpUrl(s: string): string | null {
  try {
    const u = new URL(s);
    return u.protocol === "http:" || u.protocol === "https:" ? s : null;
  } catch {
    return null;
  }
}
