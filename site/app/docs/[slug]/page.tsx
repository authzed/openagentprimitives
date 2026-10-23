import type { Metadata } from "next";
import { CopyPage } from "@/components/CopyPage";
import { JsonLd } from "@/components/JsonLd";
import { guideSlugs, loadGuide } from "@/lib/guides";
import { SITE_DESCRIPTION, SITE_NAME } from "@/lib/site";
import { guideGraph, plainText } from "@/lib/structured-data";

// Every guide is prerendered; any other slug is a real 404, not a silent
// fallback to the first guide.
export const dynamicParams = false;

export async function generateStaticParams() {
  return (await guideSlugs()).map((slug) => ({ slug }));
}

type Props = { params: Promise<{ slug: string }> };

// pnpm check requires a description on every guide; the fallback is only so a
// page under construction still renders.
const describe = (description?: string) =>
  description ? plainText(description) : SITE_DESCRIPTION;

// The share image is the sibling opengraph-image.tsx, which Next attaches to
// openGraph and twitter on its own.
export async function generateMetadata({ params }: Props): Promise<Metadata> {
  const { slug } = await params;
  const { guide } = await loadGuide(slug);
  const url = `/docs/${slug}`;
  const description = describe(guide.description);
  return {
    title: guide.title,
    description,
    alternates: {
      canonical: url,
      types: { "text/markdown": `${url}.md` },
    },
    openGraph: {
      title: `${guide.title} · OAP docs`,
      description,
      url,
      siteName: SITE_NAME,
      type: "article",
      section: guide.group || guide.section,
    },
    twitter: { card: "summary_large_image" },
  };
}

export default async function GuidePage({ params }: Props) {
  const { slug } = await params;
  const { Component, guide } = await loadGuide(slug);
  return (
    <div data-pagefind-meta={`title:${guide.title}`}>
      <JsonLd
        data={guideGraph(
          guide,
          describe(guide.description),
          `/docs/${slug}/opengraph-image`,
        )}
      />
      <CopyPage slug={slug} />
      <Component />
    </div>
  );
}
