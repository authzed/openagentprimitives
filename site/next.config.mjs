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
};

export default withMDX(nextConfig);
