import createMDX from "@next/mdx";
import { fileURLToPath } from "node:url";

// The brand marks live once, in the repo's docs/assets/brand, outside this
// package. Widening Turbopack's root (and file tracing with it) lets the site
// import them directly instead of keeping a copy that drifts.
const repoRoot = fileURLToPath(new URL("..", import.meta.url));

// Plugins are named by string: Turbopack runs the MDX compile in Rust and
// cannot receive JavaScript functions.
const withMDX = createMDX({
  options: { remarkPlugins: ["remark-gfm"] },
});

/** @type {import('next').NextConfig} */
const nextConfig = {
  pageExtensions: ["ts", "tsx", "mdx"],
  turbopack: { root: repoRoot },
  outputFileTracingRoot: repoRoot,
  // `next dev` refuses its own dev resources (the HMR socket included) to any
  // origin but localhost, and a page whose HMR socket is refused never
  // hydrates: opened at 127.0.0.1 it renders but no handler ever runs. Dev
  // only; production ignores this.
  allowedDevOrigins: ["127.0.0.1"],
};

export default withMDX(nextConfig);
