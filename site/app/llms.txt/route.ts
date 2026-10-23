import { allGuides } from "@/lib/guides";
import { llmsTxt } from "@/lib/markdown";
import { textResponse } from "@/lib/markdown-routes";
import { buildNav } from "@/lib/nav";
import { SITE_DESCRIPTION, SITE_NAME, SITE_URL } from "@/lib/site";

// The docs index for LLMs and agents (https://llmstxt.org).
export const dynamic = "force-static";

export async function GET() {
  const body = llmsTxt(buildNav(await allGuides()), {
    name: SITE_NAME,
    description: SITE_DESCRIPTION,
    siteUrl: SITE_URL,
  });
  return textResponse(body, "text/plain");
}
