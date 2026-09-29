import type { Metadata } from "next";
import { guideSlugs, loadGuide } from "@/lib/guides";
import { OG_IMAGE, SITE_NAME } from "@/lib/site";

// Every guide is prerendered; any other slug is a real 404, not a silent
// fallback to the first guide.
export const dynamicParams = false;

export async function generateStaticParams() {
  return (await guideSlugs()).map((slug) => ({ slug }));
}

type Props = { params: Promise<{ slug: string }> };

export async function generateMetadata({ params }: Props): Promise<Metadata> {
  const { slug } = await params;
  const { guide } = await loadGuide(slug);
  const url = `/docs/${slug}`;
  return {
    title: guide.title,
    description: guide.description,
    alternates: { canonical: url },
    openGraph: {
      title: `${guide.title} · OAP docs`,
      description: guide.description,
      url,
      siteName: SITE_NAME,
      type: "article",
      images: [OG_IMAGE],
    },
    twitter: { card: "summary_large_image", images: [OG_IMAGE.url] },
  };
}

export default async function GuidePage({ params }: Props) {
  const { Component, guide } = await loadGuide((await params).slug);
  return (
    <div data-pagefind-meta={`title:${guide.title}`}>
      <Component />
    </div>
  );
}
