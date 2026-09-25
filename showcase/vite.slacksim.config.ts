import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { fileURLToPath, URL } from "node:url";

// The fake-Slack simulator is a standalone single-page app. It is rendered by
// the capture engine (Playwright) which loads `?scenario=<name>&scene=<id>` and
// drives it through `window.__showcase`. It deliberately does NOT depend on
// OAP's `web/` workspace or the @ap/design system — Slack has its own look.
export default defineConfig({
  root: fileURLToPath(new URL("./demos/slacksim", import.meta.url)),
  plugins: [react()],
  resolve: {
    alias: {
      "@slacksim": fileURLToPath(
        new URL("./demos/slacksim/src", import.meta.url),
      ),
    },
  },
  build: {
    outDir: fileURLToPath(new URL("./out/slacksim", import.meta.url)),
    emptyOutDir: true,
  },
  server: {
    port: 5178,
    strictPort: true,
  },
});
