import { allGuides } from "@/lib/guides";
import { renderGuideMarkdown, textResponse } from "@/lib/markdown-routes";
import { SITE_DESCRIPTION, SITE_NAME } from "@/lib/site";

// Every guide's Markdown in one file, in sidebar order, for an agent that
// wants the whole docs set in a single fetch.
export const dynamic = "force-static";

export async function GET() {
  const pages = await Promise.all(
    (await allGuides()).map((g) => renderGuideMarkdown(g.slug)),
  );
  const body = [`# ${SITE_NAME}\n\n> ${SITE_DESCRIPTION}\n`, ...pages].join(
    "\n---\n\n",
  );
  return textResponse(body, "text/plain");
}
