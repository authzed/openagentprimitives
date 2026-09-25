import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";
import path from "node:path";
import fs from "node:fs";
import fg from "fast-glob";

const repoRoot = path.resolve(process.cwd(), "..");

// discoverEntries maps appKey -> entry index.tsx by reading each pkg/**/ui/app.json,
// plus the built-in `system` app. Throws on a missing key / index.tsx / duplicate.
function discoverEntries(): Record<string, string> {
  const input: Record<string, string> = {};
  for (const aj of fg.sync("pkg/**/ui/**/app.json", {
    cwd: repoRoot,
    absolute: true,
  })) {
    const key = (JSON.parse(fs.readFileSync(aj, "utf8")) as { key?: string })
      .key;
    if (!key) throw new Error(`app.json missing "key": ${aj}`);
    const entry = path.join(path.dirname(aj), "index.tsx");
    if (!fs.existsSync(entry)) throw new Error(`no index.tsx next to ${aj}`);
    if (input[key]) throw new Error(`duplicate appKey "${key}"`);
    input[key] = entry;
  }
  input["system"] = path.resolve(
    process.cwd(),
    "packages/runtime/src/system/index.tsx",
  );
  return input;
}

// manifestPlugin emits dist/manifest.json: appKey -> { scripts:[entry], css:[...] }.
// The entry statically imports the shared `vendor` chunk, so only the entry script
// tag is required; CSS is collected across the entry + its imported chunks.
function manifestPlugin(): Plugin {
  return {
    name: "ap-manifest",
    generateBundle(_opts, bundle) {
      const chunks: Record<string, any> = {};
      for (const f of Object.values(bundle))
        if ((f as any).type === "chunk") chunks[(f as any).fileName] = f;
      const out: Record<string, { scripts: string[]; css: string[] }> = {};
      for (const f of Object.values(bundle) as any[]) {
        if (f.type !== "chunk" || !f.isEntry) continue;
        const css = new Set<string>();
        const seen = new Set<string>();
        const visit = (c: any) => {
          if (!c || seen.has(c.fileName)) return;
          seen.add(c.fileName);
          for (const cssFile of c.viteMetadata?.importedCss ?? [])
            css.add("/assets/" + cssFile);
          for (const imp of c.imports ?? []) visit(chunks[imp]);
        };
        visit(f);
        out[f.name] = { scripts: ["/assets/" + f.fileName], css: [...css] };
      }
      this.emitFile({
        type: "asset",
        fileName: "manifest.json",
        source: JSON.stringify(out, null, 2),
      });
    },
  };
}

export default defineConfig({
  // All bundles + assets (incl. fonts via css url()) and cross-chunk import
  // statements are emitted under /assets/, matching the Go static route. REQUIRED.
  base: "/assets/",
  // Never wipe the terminal in dev: when run through `mage web:dev` (mage → pnpm
  // → vite) the post-pre-bundle screen-clear hides the "ready" banner, leaving
  // the user staring at the esbuild step — looks hung. Keep all output visible.
  clearScreen: false,
  plugins: [react(), manifestPlugin()],
  resolve: {
    alias: {
      "@ap/agentui": path.resolve(process.cwd(), "packages/agentui/src"),
      "@ap/design": path.resolve(process.cwd(), "packages/design/src"),
      "@ap/runtime": path.resolve(process.cwd(), "packages/runtime/src"),
      react: path.resolve(process.cwd(), "node_modules/react"),
      "react-dom": path.resolve(process.cwd(), "node_modules/react-dom"),
      "react/jsx-runtime": path.resolve(
        process.cwd(),
        "node_modules/react/jsx-runtime",
      ),
      "lucide-react": path.resolve(process.cwd(), "node_modules/lucide-react"),
      anser: path.resolve(process.cwd(), "node_modules/anser"),
      "@mcp-ui/client": path.resolve(
        process.cwd(),
        "node_modules/@mcp-ui/client",
      ),
    },
  },
  build: {
    // Write the committed build into the PARENT Go module so the embed package
    // (pkg/web/webui/webassets) can //go:embed it. web/ is a nested module excluded
    // from the parent ./... walk.
    outDir: path.resolve(process.cwd(), "../pkg/web/webui/webassets/dist"),
    emptyOutDir: true,
    rollupOptions: {
      input: discoverEntries(),
      output: {
        entryFileNames: "[name].[hash].js",
        chunkFileNames: "[name].[hash].js",
        assetFileNames: "[name].[hash].[ext]",
        manualChunks(id) {
          // Vendor only third-party deps. Do NOT sweep /web/packages/ here: the
          // `system` entry lives under web/packages/runtime and forcing an entry
          // module into a manual chunk breaks it as an entry point.
          if (id.includes("node_modules")) return "vendor";
          return undefined;
        },
      },
    },
  },
  server: { fs: { allow: [repoRoot] } },
});
