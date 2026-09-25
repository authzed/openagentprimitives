import * as React from "react";
import { Check, Copy } from "lucide-react";

// CopyButton is the icon-only copy-to-clipboard control, flipping to a check for
// ~1.2s. The clipboard call is guarded: jsdom (and insecure origins) may lack
// navigator.clipboard entirely, so a missing API is a no-op, never a throw.
export function CopyButton({ text }: { text: string }) {
  const [copied, setCopied] = React.useState(false);
  const onCopy = () => {
    const p = navigator.clipboard?.writeText(text);
    if (p && typeof p.then === "function") {
      p.then(() => { setCopied(true); setTimeout(() => setCopied(false), 1200); }).catch(() => {});
    }
  };
  return (
    <button
      type="button"
      aria-label="Copy command"
      onClick={onCopy}
      className="inline-flex h-6 w-6 shrink-0 items-center justify-center rounded border border-input bg-background text-muted-foreground hover:bg-accent hover:text-accent-foreground"
    >
      {copied ? <Check className="h-3 w-3 text-success" /> : <Copy className="h-3 w-3" />}
    </button>
  );
}

// CopyCmd shows a read-only command in a bordered box with a CopyButton.
export function CopyCmd({ text }: { text: string }) {
  return (
    <div className="flex items-center gap-2 rounded-md border bg-background px-3 py-2">
      <code className="flex-1 truncate font-mono text-xs text-foreground" title={text}>{text}</code>
      <CopyButton text={text} />
    </div>
  );
}
