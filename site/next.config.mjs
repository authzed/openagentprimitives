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

// Response headers for every route. The site is fully static, so the CSP
// cannot carry per-request nonces: Next's hydration payload and next-themes'
// pre-paint script are inline, hence 'unsafe-inline' for scripts. Pagefind
// compiles WebAssembly ('wasm-unsafe-eval'), and analytics posts to
// i.authzed.com. The CSP is production-only because `next dev` relies on eval.
const csp = [
  "default-src 'self'",
  "script-src 'self' 'unsafe-inline' 'wasm-unsafe-eval'",
  "style-src 'self' 'unsafe-inline'",
  "img-src 'self' data: blob:",
  "media-src 'self'",
  "font-src 'self'",
  "connect-src 'self' https://i.authzed.com",
  "object-src 'none'",
  "base-uri 'self'",
  "form-action 'self'",
  "frame-ancestors 'none'",
  "upgrade-insecure-requests",
].join("; ");

const securityHeaders = [
  {
    key: "Strict-Transport-Security",
    value: "max-age=63072000; includeSubDomains",
  },
  { key: "X-Content-Type-Options", value: "nosniff" },
  { key: "X-Frame-Options", value: "DENY" },
  { key: "Referrer-Policy", value: "strict-origin-when-cross-origin" },
  {
    key: "Permissions-Policy",
    value: "camera=(), microphone=(), geolocation=()",
  },
  ...(process.env.NODE_ENV === "production"
    ? [{ key: "Content-Security-Policy", value: csp }]
    : []),
];

/** @type {import('next').NextConfig} */
const nextConfig = {
  async headers() {
    return [{ source: "/:path*", headers: securityHeaders }];
  },
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
