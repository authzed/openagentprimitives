// schema.org JSON-LD for search engines and agents, rendered by
// components/JsonLd.tsx. Pure builders, so the shapes are tested directly.
//
// Every page emits one @graph. The shared nodes (organization, website, the
// project) carry a stable @id and are linked by it. Consumers resolve @id only
// within a page, so every page that links one also includes it.
import type { MediaAsset } from "./manifest";
import type { Guide } from "./nav";
import {
  PUBLISHER,
  REPO_URL,
  SITE_DESCRIPTION,
  SITE_NAME,
  SITE_URL,
} from "./site";

const ORG_ID = `${PUBLISHER.url}/#organization`;
const WEBSITE_ID = `${SITE_URL}/#website`;
const SOFTWARE_ID = `${SITE_URL}/#software`;

type Node = Record<string, unknown>;

export function graph(...nodes: Node[]): Node {
  return { "@context": "https://schema.org", "@graph": nodes };
}

export function organization(): Node {
  return {
    "@type": "Organization",
    "@id": ORG_ID,
    name: PUBLISHER.name,
    url: PUBLISHER.url,
    sameAs: ["https://github.com/authzed"],
  };
}

export function website(): Node {
  return {
    "@type": "WebSite",
    "@id": WEBSITE_ID,
    name: SITE_NAME,
    alternateName: "OAP",
    url: `${SITE_URL}/`,
    description: SITE_DESCRIPTION,
    inLanguage: "en",
    publisher: { "@id": ORG_ID },
  };
}

// The project itself. SoftwareSourceCode rather than SoftwareApplication: the
// latter's rich result wants ratings and a price, and OAP has neither to
// state honestly.
export function software(): Node {
  return {
    "@type": "SoftwareSourceCode",
    "@id": SOFTWARE_ID,
    name: SITE_NAME,
    alternateName: "OAP",
    description: SITE_DESCRIPTION,
    url: `${SITE_URL}/`,
    codeRepository: REPO_URL,
    license: "https://www.apache.org/licenses/LICENSE-2.0",
    programmingLanguage: "Go",
    runtimePlatform: "Kubernetes",
    keywords: [
      "AI agents",
      "agent security",
      "authorization",
      "SpiceDB",
      "Kubernetes",
      "MCP",
    ],
    author: { "@id": ORG_ID },
    publisher: { "@id": ORG_ID },
  };
}

export function landingGraph(): Node {
  return graph(organization(), website(), software());
}

// A docs page: the article, and the breadcrumb a result shows in place of the
// URL. Sections and groups have no page of their own, so the trail is
// Home › Docs › page; a crumb without a URL is not allowed except last.
export function guideGraph(
  guide: Guide,
  description: string,
  image: string,
): Node {
  const url = `${SITE_URL}/docs/${guide.slug}`;
  return graph(
    organization(),
    website(),
    software(),
    {
      "@type": "TechArticle",
      "@id": `${url}#article`,
      headline: guide.title,
      description,
      url,
      mainEntityOfPage: url,
      image: `${SITE_URL}${image}`,
      articleSection: guide.group || guide.section,
      inLanguage: "en",
      isPartOf: { "@id": WEBSITE_ID },
      about: { "@id": SOFTWARE_ID },
      publisher: { "@id": ORG_ID },
    },
    {
      "@type": "BreadcrumbList",
      itemListElement: [
        { name: SITE_NAME, item: `${SITE_URL}/` },
        { name: "Docs", item: `${SITE_URL}/docs` },
        { name: guide.title, item: url },
      ].map((crumb, i) => ({ "@type": "ListItem", position: i + 1, ...crumb })),
    },
  );
}

// Meta descriptions are plain text; guide descriptions may carry Markdown
// code spans or emphasis (`latest`, *external*).
export function plainText(s: string): string {
  return s
    .replace(/`([^`]*)`/g, "$1")
    .replace(/\*\*([^*]+)\*\*/g, "$1")
    .replace(/\*([^*]+)\*/g, "$1");
}

export interface Story {
  title: string;
  description: string;
  /** Site-relative path, e.g. "/stories/claude-design". */
  path: string;
  keywords: string[];
  published: string;
  updated?: string;
  /** Site-relative share image. */
  image: string;
}

export interface FaqEntry {
  q: string;
  /** Plain text; `code spans` are stripped for the structured copy. */
  a: string;
}

// A story page: the article, its FAQ, its demo video when the clip carries
// everything VideoObject requires, and the breadcrumb.
export function storyGraph(
  story: Story,
  faq: FaqEntry[],
  clip?: MediaAsset,
): Node {
  const url = `${SITE_URL}${story.path}`;
  return graph(
    organization(),
    website(),
    software(),
    {
      "@type": "TechArticle",
      "@id": `${url}#article`,
      headline: story.title,
      description: story.description,
      url,
      mainEntityOfPage: url,
      image: `${SITE_URL}${story.image}`,
      keywords: story.keywords.join(", "),
      datePublished: story.published,
      dateModified: story.updated ?? story.published,
      inLanguage: "en",
      isPartOf: { "@id": WEBSITE_ID },
      about: { "@id": SOFTWARE_ID },
      publisher: { "@id": ORG_ID },
    },
    {
      "@type": "FAQPage",
      "@id": `${url}#faq`,
      mainEntity: faq.map((f) => ({
        "@type": "Question",
        name: f.q,
        acceptedAnswer: { "@type": "Answer", text: plainText(f.a) },
      })),
    },
    ...(clip ? videoObject(clip) : []),
    {
      "@type": "BreadcrumbList",
      itemListElement: [
        { name: SITE_NAME, item: `${SITE_URL}/` },
        { name: story.title, item: url },
      ].map((crumb, i) => ({ "@type": "ListItem", position: i + 1, ...crumb })),
    },
  );
}

// A VideoObject only when the clip carries every field the type requires; a
// partial one is flagged by validators and earns nothing.
function videoObject(clip: MediaAsset): Node[] {
  const content = clip.mp4 ?? clip.webm;
  if (
    !clip.title ||
    !clip.caption ||
    !clip.poster ||
    !clip.uploadDate ||
    !content
  )
    return [];
  return [
    {
      "@type": "VideoObject",
      name: clip.title,
      description: clip.caption,
      thumbnailUrl: `${SITE_URL}${clip.poster}`,
      uploadDate: clip.uploadDate,
      contentUrl: `${SITE_URL}${content}`,
      ...(clip.duration ? { duration: clip.duration } : {}),
      ...(clip.transcript?.length
        ? { transcript: clip.transcript.join(" ") }
        : {}),
      publisher: { "@id": ORG_ID },
    },
  ];
}
