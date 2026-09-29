import type { Metadata } from "next";
import { guideSlugs, loadGuide } from "@/lib/guides";

// Every guide is prerendered; any other slug is a real 404, not a silent
// fallback to the first guide.
export const dynamicParams = false;

export async function generateStaticParams() {
  return (await guideSlugs()).map((slug) => ({ slug }));
}

type Props = { params: Promise<{ slug: string }> };

export async function generateMetadata({ params }: Props): Promise<Metadata> {
  const { guide } = await loadGuide((await params).slug);
  return { title: guide.title, description: guide.description };
}

export default async function GuidePage({ params }: Props) {
  const { Component, guide } = await loadGuide((await params).slug);
  return (
    <div data-pagefind-meta={`title:${guide.title}`}>
      <Component />
    </div>
  );
}
