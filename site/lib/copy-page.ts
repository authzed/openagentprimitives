// The docs "Copy page" menu's destinations (components/CopyPage.tsx). Pure, so
// the URLs are tested without a browser.
//
// The chat links hand the assistant the page's Markdown rendition rather than
// its HTML: it is the same content without the sidebar, scripts and styles,
// so the model reads the guide and not the chrome around it.

export function markdownPath(slug: string): string {
  return `/docs/${slug}.md`;
}

export function askPrompt(markdownUrl: string): string {
  return `Read from ${markdownUrl} so I can ask questions about it.`;
}

export interface ChatLink {
  id: "chatgpt" | "claude";
  label: string;
  href: string;
}

export function chatLinks(markdownUrl: string): ChatLink[] {
  const q = encodeURIComponent(askPrompt(markdownUrl));
  return [
    {
      id: "chatgpt",
      label: "Open in ChatGPT",
      href: `https://chatgpt.com/?hints=search&prompt=${q}`,
    },
    {
      id: "claude",
      label: "Open in Claude",
      href: `https://claude.ai/new?q=${q}`,
    },
  ];
}
