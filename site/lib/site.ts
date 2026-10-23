// The production origin. Canonical URLs, Open Graph URLs, the sitemap and
// robots.txt all resolve against it, including on preview deploys, so shared
// links and search results always point at the real site.
export const SITE_URL = "https://openap.org";

export const SITE_NAME = "Open Agent Primitives";

// Mirrors the landing hero (app/(landing)/Landing.tsx). Change them together.
export const SITE_DESCRIPTION =
  "Open-source building blocks for running enterprise AI agents in your own Kubernetes cluster, on the models you choose. OAP checks every action before it runs.";

// The landing page's <title>, which is also its share-card headline.
export const SITE_TITLE =
  "Open Agent Primitives: a secure way to run enterprise AI agents";

export const REPO_URL = "https://github.com/authzed/openagentprimitives";

export const PUBLISHER = {
  name: "AuthZed",
  url: "https://authzed.com",
} as const;

// The build-time share card (app/opengraph-image.tsx). A page that sets its
// own openGraph replaces the inherited one wholesale, image included, so
// those pages list it again explicitly.
export const OG_IMAGE = {
  url: "/opengraph-image",
  width: 1200,
  height: 630,
  alt: "Open Agent Primitives: a secure way to run enterprise AI agents",
};

// The AuthZed Discord, the destination authzed.com/discord redirects to.
export const DISCORD_URL = "https://discord.gg/TUd4k5McMX";
