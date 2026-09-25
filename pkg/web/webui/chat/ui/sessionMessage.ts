// sessionPath builds the session-scoped API prefix every request in this
// package hangs off. Both halves are percent-encoded because each is ONE path
// segment: a namespace or name must never be able to introduce another, and
// the segment pair is exactly what the route-level Authorize gate reads
// (pkg/web/webui/chat's Routes), so what the browser asks for and what the
// gate checked cannot disagree.
export function sessionPath(ns: string, name: string): string {
  return `/sessions/api/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`;
}

// failureText turns a non-2xx response into the one line the viewer sees.
//
// A non-2xx on these endpoints is NOT reliably the JSON `{"error": ...}` a
// chat handler writes: the route-level Authorize gate answers an unauthorized
// or indeterminate caller with an HTML system page BEFORE any handler runs,
// and 401/403/503 are produced by both paths. So the body is read as JSON only
// when the response declares itself JSON — reading an HTML page as JSON throws,
// and a parse error thrown inside a fetch handler surfaces as a blank or stuck
// view instead of telling the viewer they do not have access.
//
// `fallback` is the caller's own copy for the case where the server supplied
// no usable message; it always names what failed.
export async function failureText(resp: Response, fallback: string): Promise<string> {
  if ((resp.headers.get("content-type") ?? "").includes("application/json")) {
    try {
      const body = (await resp.json()) as { error?: string } | null;
      const msg = body?.error;
      if (typeof msg === "string" && msg.trim() !== "") return msg;
    } catch {
      // The response claimed JSON and was not. Nothing is dropped here: the
      // status-derived line below IS this path's user-visible answer, and every
      // caller logs the status alongside it.
    }
  }
  switch (resp.status) {
    case 401:
      return "You are no longer signed in. Reload the page to continue.";
    case 403:
      return "You do not have access to this conversation.";
    case 503:
      return "That could not be checked right now. Try again in a moment.";
    default:
      return fallback;
  }
}

// sendSessionMessage sends one message to the session's transcript, exactly as
// the chat composer does — the same route, the same cookie, the same interact
// check, attributed to the same viewer.
//
// That equivalence is the whole safety argument for any surface that composes
// a message on the viewer's behalf: the message is one the viewer could have
// typed themselves, so composing it from a declaration grants nobody anything.
// It also means the request is VISIBLE — it lands in the transcript, where a
// viewer can see what was asked on their behalf and read the answer, rather
// than a button quietly starting a conversation they never see.
//
// It lives beside the transcript it writes to, not beside any one caller, for
// the same reason: every surface that speaks for the viewer must speak through
// this one route, and a copy per caller is how one of them quietly stops.
export async function sendSessionMessage(ns: string, name: string, text: string): Promise<void> {
  const res = await fetch(`/sessions/api/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/message`, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ text }),
  });
  if (!res.ok) throw new Error(`send session message: request failed (${res.status})`);
}
