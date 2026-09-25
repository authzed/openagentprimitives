export interface Notice {
  kind: "success" | "error";
  text: string;
}

// parseNotice reads a ?notice=<verb>:<cred> (success) or ?error=<message>
// query (set by the action-handler redirects) into a display Notice.
export function parseNotice(search: string): Notice | null {
  const q = new URLSearchParams(search);
  const err = q.get("error");
  if (err) return { kind: "error", text: err };
  const notice = q.get("notice");
  if (notice) {
    const [verb, cred] = notice.split(":");
    if (verb === "linked" && cred) return { kind: "success", text: `Linked ${cred}.` };
    if (verb === "revoked" && cred) return { kind: "success", text: `Revoked ${cred}.` };
    if (verb === "linkedunverified" && cred)
      return { kind: "error", text: `Linked ${cred} — but the token could not be verified. It may not work.` };
    return { kind: "success", text: notice };
  }
  return null;
}
