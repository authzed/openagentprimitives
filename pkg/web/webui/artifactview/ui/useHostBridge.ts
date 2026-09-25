import { useCallback, useEffect, useRef } from "react";

// The host iframe is cross-origin (sandbox origin); the shell drives revisions by
// postMessage to it, origin-targeted to sandboxOrigin. The host owns scroll
// preservation same-origin (see host.go). First framing sets the iframe src to
// the host URL (the host self-embeds the initial content); later revisions swap
// the host's INNER frame via a swap command.
interface FrameLike {
  getAttribute(name: string): string | null;
  setAttribute(name: string, value: string): void;
  contentWindow: { postMessage: (msg: unknown, target: string) => void } | null;
}

export interface RevisionURLs {
  hostUrl?: string;
  contentUrl?: string;
}

export function hostBridge(frame: FrameLike, sandboxOrigin: string) {
  function showRevision(u: RevisionURLs) {
    const framed = !!frame.getAttribute("src");
    if (!framed) {
      if (u.hostUrl) frame.setAttribute("src", u.hostUrl);
      return;
    }
    if (u.contentUrl) {
      try {
        frame.contentWindow?.postMessage({ ap: "host", cmd: "swap", url: u.contentUrl }, sandboxOrigin);
      } catch {
        /* host not ready yet; the next revision push retries */
      }
    }
  }
  return { showRevision };
}

export function useHostBridge(iframeRef: React.RefObject<HTMLIFrameElement>, sandboxOrigin: string) {
  const bridgeRef = useRef<ReturnType<typeof hostBridge> | null>(null);
  useEffect(() => {
    if (iframeRef.current) bridgeRef.current = hostBridge(iframeRef.current as unknown as FrameLike, sandboxOrigin);
  }, [iframeRef, sandboxOrigin]);
  return useCallback((u: RevisionURLs) => bridgeRef.current?.showRevision(u), []);
}
