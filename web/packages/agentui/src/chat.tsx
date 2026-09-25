// chat.tsx renders ap:chat — the browser chat for one session, inside the
// page, as a same-origin iframe of /chat-embed/{ns}/{name}. That page checks
// the VIEWER's own interact permission itself, which is why the iframe has no
// sandbox attribute (same argument as ap:session_view): the framed page is
// the enforcement, and there is no separate grant to narrow.
import { Alert } from "@ap/design";
import { p } from "./props";
import { sessionRefSegments } from "./sessionRef";
import type { Node } from "./types";

// chatSrc is sessionViewSrc's rule with this page's own prefix: the shared
// validator (sessionRef.ts) decides what a session reference may be, and all
// this adds is /chat-embed and the per-segment encode.
export function chatSrc(ref: unknown): string | null {
  const segs = sessionRefSegments(ref);
  if (segs === null) return null;
  return `/chat-embed/${encodeURIComponent(segs[0])}/${encodeURIComponent(segs[1])}`;
}

export function ChatFrame({
  sessionRef,
}: {
  sessionRef: unknown;
}): JSX.Element {
  const src = chatSrc(sessionRef);
  if (src === null) {
    return <Alert>Chat unavailable: no valid session reference.</Alert>;
  }
  return (
    <iframe
      data-testid="ap-chat"
      src={src}
      title="Test chat"
      className="w-full rounded-md bg-background"
      style={{ height: "520px", border: "0" }}
    />
  );
}

export function ChatNode({ n }: { n: Node }): JSX.Element {
  return <ChatFrame sessionRef={p(n).sessionRef} />;
}
