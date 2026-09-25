// decodeSubject turns a canonical SpiceDB subject back into the human email it
// encodes. A platform user subject is `user:` + base64url(lower(email)); groups
// and service identities are plain names (`group:platform-ops#member`,
// `service:hubspot-bot`) that are NOT encoded. So this strips a leading
// `user:` / `group:` prefix, base64url-decodes the rest as UTF-8, and returns
// the decoded value ONLY when it looks like an email (contains "@"). On any
// failure — not base64url, not valid UTF-8, or not an email — it returns the
// raw subject unchanged, so non-encoded subjects pass through verbatim.

// base64UrlToBytes decodes a base64url string to bytes, throwing (via atob) on
// any character outside the base64url alphabet. Padding is re-added since the
// canonical form omits it.
function base64UrlToBytes(s: string): Uint8Array {
  const b64 = s.replace(/-/g, "+").replace(/_/g, "/");
  const pad = b64.length % 4 === 0 ? "" : "=".repeat(4 - (b64.length % 4));
  const bin = atob(b64 + pad);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return bytes;
}

export function decodeSubject(subject: string): string {
  if (!subject) return subject;
  const m = /^(?:user|group):(.*)$/.exec(subject);
  const rest = m ? m[1] : subject;
  if (rest === "") return subject;
  try {
    const decoded = new TextDecoder("utf-8", { fatal: true }).decode(base64UrlToBytes(rest));
    // Canonical user subjects encode an email; anything else decoded is almost
    // certainly a coincidental base64url name — keep the raw subject for it.
    return decoded.includes("@") ? decoded : subject;
  } catch {
    return subject;
  }
}
