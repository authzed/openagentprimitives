import { defineConfig } from "vite";
import { fileURLToPath, URL } from "node:url";

// The fake-console simulator: a standalone single-page app that renders a themed
// xterm.js terminal and scripts it (animated typing + ANSI-colored output). The
// capture engine (Playwright) loads `?scenario=<name>&theme=<light|dark>` and
// drives it through the same `window.__showcase` / `window.__showcaseStory`
// contract as slacksim — so a CLI flow captures to stills + narrated clips too.
export default defineConfig({
  root: fileURLToPath(new URL("./demos/consolesim", import.meta.url)),
  build: {
    outDir: fileURLToPath(new URL("./out/consolesim", import.meta.url)),
    emptyOutDir: true,
  },
  server: { port: 5180, strictPort: true },
});
