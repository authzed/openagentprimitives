import { ImageResponse } from "next/og";
import { nbWrap, OG_INK, OG_SIZE, OgCard } from "@/components/OgCard";
import { guideSlugs, loadGuide } from "@/lib/guides";
import { plainText } from "@/lib/structured-data";

// One share card per guide, so a docs link pasted into Slack or a search
// result shows which page it is, not the site-wide card. Prerendered for
// every guide. The alt is static: Next 16 hands generateImageMetadata no
// params under a dynamic segment at build time, so it cannot name the page.
export const size = OG_SIZE;
export const contentType = "image/png";
export const alt = "An Open Agent Primitives docs page title and summary";

// Like the page and its Markdown route: an unknown slug is a 404, not a
// failed import and a 500.
export const dynamicParams = false;

export async function generateStaticParams() {
  return (await guideSlugs()).map((slug) => ({ slug }));
}

// Two lines of description at this size; longer ones end in an ellipsis.
const DESC_MAX = 150;

export default async function GuideOgImage({
  params,
}: {
  params: Promise<{ slug: string }>;
}) {
  const { guide } = await loadGuide((await params).slug);
  const kicker = guide.group
    ? `${guide.section} · ${guide.group}`
    : guide.section;
  let desc = plainText(guide.description ?? "");
  if (desc.length > DESC_MAX)
    desc = desc.slice(0, desc.lastIndexOf(" ", DESC_MAX)) + "…";
  return new ImageResponse(
    <OgCard footer="Open Agent Primitives docs · openap.org">
      <div style={{ fontSize: 28, color: OG_INK.accent, marginBottom: 20 }}>
        {nbWrap(kicker)}
      </div>
      <div
        style={{
          fontSize: guide.title.length > 28 ? 60 : 72,
          lineHeight: 1.08,
          letterSpacing: "-0.03em",
        }}
      >
        {nbWrap(guide.title)}
      </div>
      {desc && (
        <div
          style={{
            fontSize: 30,
            lineHeight: 1.35,
            color: OG_INK.muted,
            marginTop: 24,
          }}
        >
          {nbWrap(desc)}
        </div>
      )}
    </OgCard>,
    size,
  );
}
