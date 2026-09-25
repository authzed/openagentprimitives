import { defineConfig } from "vite";
import path from "node:path";

// Standalone harness config. process.cwd() is web/ (pnpm -C web runs vite there).
// root points at this dir; base "/" keeps URLs clean; fs.allow includes the repo
// root so the harness can import the host modules that live under pkg/ (outside
// web/). No app.json here, and a separate config — so the production build
// (web/vite.config.ts) never sees the harness.
const cwd = process.cwd();
export default defineConfig({
  root: path.resolve(cwd, "dev/annotator"),
  base: "/",
  clearScreen: false,
  server: {
    port: 5174,
    open: true,
    fs: { allow: [path.resolve(cwd, "..")] },
  },
});
