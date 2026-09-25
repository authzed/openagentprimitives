import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { cn } from "../../lib/utils";

// Markdown renders agent/user text as GitHub-flavored markdown — headings,
// lists, links, GFM tables, and fenced code blocks — shared by every chat
// surface (the web chat and the artifact-view chat panel) so they render
// identically.
//
// Injection safety: raw HTML in the source is NOT rendered. react-markdown
// escapes HTML by default and we deliberately do NOT add rehype-raw, so agent
// or user text can never inject markup or scripts — the same guarantee the
// previous plain-text renderer had. Links get target="_blank" +
// rel="noopener noreferrer", and react-markdown's default urlTransform strips
// dangerous URL protocols (javascript:, data:, …).
//
// Styling is done with Tailwind arbitrary-variant classes on the wrapper (no
// typography plugin, no inline styles) so it works under the artifact view's
// strict style-src CSP. Wide tables and code blocks scroll inside their own
// overflow-x container rather than widening the bubble.
export function Markdown({
  children,
  className,
}: {
  children: string;
  className?: string;
}) {
  return (
    <div
      className={cn(
        // Font size is inherited from the surrounding bubble; only line-height
        // and wrapping are imposed here so the component is context-agnostic.
        "leading-relaxed break-words",
        "[&>*:first-child]:mt-0 [&>*:last-child]:mb-0",
        "[&_p]:my-1.5",
        "[&_ul]:my-1.5 [&_ul]:list-disc [&_ul]:pl-5 [&_ol]:my-1.5 [&_ol]:list-decimal [&_ol]:pl-5 [&_li]:my-0.5",
        "[&_h1]:mb-1.5 [&_h1]:mt-2 [&_h1]:text-base [&_h1]:font-semibold",
        "[&_h2]:mb-1.5 [&_h2]:mt-2 [&_h2]:text-[0.95rem] [&_h2]:font-semibold",
        "[&_h3]:mb-1 [&_h3]:mt-2 [&_h3]:font-semibold",
        "[&_a]:underline [&_a]:underline-offset-2 hover:[&_a]:no-underline",
        "[&_blockquote]:my-1.5 [&_blockquote]:border-l-2 [&_blockquote]:border-border [&_blockquote]:pl-2.5 [&_blockquote]:text-muted-foreground",
        "[&_hr]:my-2 [&_hr]:border-border",
        // Inline code (a <code> NOT inside a <pre>): subtle chip.
        "[&_:not(pre)>code]:rounded [&_:not(pre)>code]:bg-black/10 [&_:not(pre)>code]:px-1 [&_:not(pre)>code]:py-0.5 [&_:not(pre)>code]:font-mono [&_:not(pre)>code]:text-[0.85em] dark:[&_:not(pre)>code]:bg-white/15",
        // Fenced code block: monospace card that scrolls horizontally.
        "[&_pre]:my-1.5 [&_pre]:overflow-x-auto [&_pre]:rounded-md [&_pre]:bg-black/10 [&_pre]:p-2.5 [&_pre]:text-[0.8rem] dark:[&_pre]:bg-white/10",
        "[&_pre_code]:font-mono [&_pre_code]:whitespace-pre",
        // GFM tables.
        "[&_table]:w-full [&_table]:border-collapse [&_table]:text-xs",
        "[&_th]:border [&_th]:border-border [&_th]:bg-muted [&_th]:px-2 [&_th]:py-1 [&_th]:text-left [&_th]:font-semibold",
        "[&_td]:border [&_td]:border-border [&_td]:px-2 [&_td]:py-1 [&_td]:align-top",
        className,
      )}
    >
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        components={{
          // Open links in a new tab, safely. Drop react-markdown's `node` prop
          // so it isn't spread onto the DOM element.
          a: ({ node: _node, ...props }) => (
            <a {...props} target="_blank" rel="noopener noreferrer" />
          ),
          // Wrap tables so a wide table scrolls inside its own container rather
          // than widening the chat bubble / page.
          table: ({ node: _node, ...props }) => (
            <div className="my-1.5 overflow-x-auto">
              <table {...props} />
            </div>
          ),
        }}
      >
        {children}
      </ReactMarkdown>
    </div>
  );
}
