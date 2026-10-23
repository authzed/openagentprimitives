import { guideSlugs } from "@/lib/guides";
import { renderGuideMarkdown, textResponse } from "@/lib/markdown-routes";
import { SITE_URL } from "@/lib/site";

// The Markdown rendition of one guide. Reached as /docs/<slug>.md, or as
// /docs/<slug> with `Accept: text/markdown`; both are rewrites in
// next.config.mjs. Prerendered like the HTML pages.
export const dynamicParams = false;

export async function generateStaticParams() {
  return (await guideSlugs()).map((slug) => ({ slug }));
}

export async function GET(
  _req: Request,
  { params }: { params: Promise<{ slug: string }> },
) {
  const { slug } = await params;
  return textResponse(
    await renderGuideMarkdown(slug),
    "text/markdown",
    `${SITE_URL}/docs/${slug}`,
  );
}
