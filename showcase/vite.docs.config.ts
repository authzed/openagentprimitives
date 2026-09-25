import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import mdx from "@mdx-js/rollup";
import remarkGfm from "remark-gfm";
import { fileURLToPath, URL } from "node:url";

// The docs site: MDX guides rendered by a small React app. Media (clips,
// screenshots) is served from docs/public and referenced by name through
// docs/_manifest.json. Guides carry code-anchored claims in sibling .refs.yaml
// sidecars, validated by docs/check.ts (not part of the Vite build).
export default defineConfig({
  root: fileURLToPath(new URL("./docs/app", import.meta.url)),
  publicDir: fileURLToPath(new URL("./docs/public", import.meta.url)),
  plugins: [
    {
      enforce: "pre",
      ...mdx({
        remarkPlugins: [remarkGfm],
        providerImportSource: "@mdx-js/react",
      }),
    },
    react(),
  ],
  build: {
    outDir: fileURLToPath(new URL("./out/docs", import.meta.url)),
    emptyOutDir: true,
  },
  // The brand marks are imported from the repo root's docs/assets/brand, which
  // sits outside this package; let the dev server read it.
  server: {
    port: 5179,
    strictPort: true,
    fs: {
      allow: [
        fileURLToPath(new URL(".", import.meta.url)),
        fileURLToPath(new URL("../docs/assets/brand", import.meta.url)),
      ],
    },
  },
});
