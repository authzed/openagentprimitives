// The production origin. Canonical URLs, Open Graph URLs, the sitemap and
// robots.txt all resolve against it, including on preview deploys, so shared
// links and search results always point at the real site.
export const SITE_URL = "https://openap.org";

export const SITE_NAME = "Open Agent Primitives";

export const SITE_DESCRIPTION =
  "Building blocks for running enterprise AI agents in your own cluster. The AI never decides what it's allowed to do: OAP checks every action before it runs.";

// The build-time share card (app/opengraph-image.tsx). A page that sets its
// own openGraph replaces the inherited one wholesale, image included, so
// those pages list it again explicitly.
export const OG_IMAGE = {
  url: "/opengraph-image",
  width: 1200,
  height: 630,
  alt: "Open Agent Primitives: a secure way to run enterprise AI agents",
};
