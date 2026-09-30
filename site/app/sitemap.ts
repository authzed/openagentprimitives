import type { MetadataRoute } from "next";
import { guideSlugs } from "@/lib/guides";
import { SITE_URL } from "@/lib/site";

export default async function sitemap(): Promise<MetadataRoute.Sitemap> {
  const slugs = await guideSlugs();
  return [
    { url: `${SITE_URL}/`, priority: 1 },
    ...slugs.map((slug) => ({ url: `${SITE_URL}/docs/${slug}` })),
  ];
}
