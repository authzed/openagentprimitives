import "server-only";
import { allGuides, guideSource } from "./guides";
import { getAsset } from "./manifest";
import { guideToMarkdown, type MarkdownContext } from "./markdown";
import { SITE_URL } from "./site";

// Shared by the route handlers that serve docs as text: app/md/[slug],
// app/llms.txt, and app/llms-full.txt.
const ctx: MarkdownContext = { siteUrl: SITE_URL, asset: getAsset };

export async function renderGuideMarkdown(slug: string): Promise<string> {
  const guide = (await allGuides()).find((g) => g.slug === slug);
  if (!guide) throw new Error(`no guide ${slug}`);
  return guideToMarkdown(guide, await guideSource(slug), ctx);
}

// Text responses are for agents, not search engines: they stay out of the
// index, and a page's Markdown names its HTML page as the canonical copy.
export function textResponse(
  body: string,
  contentType: string,
  canonical?: string,
): Response {
  const headers: Record<string, string> = {
    "Content-Type": `${contentType}; charset=utf-8`,
    "X-Robots-Tag": "noindex",
  };
  if (canonical) headers.Link = `<${canonical}>; rel="canonical"`;
  return new Response(body, { headers });
}
