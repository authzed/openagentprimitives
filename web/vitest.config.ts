import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import path from "node:path";

const r = (p: string) => path.resolve(process.cwd(), p);

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      "@ap/agentui": r("packages/agentui/src"),
      "@ap/design": r("packages/design/src"),
      "@ap/runtime": r("packages/runtime/src"),
      // Co-located plugin tests live outside web/, so bare deps must resolve to
      // web/node_modules regardless of the test file's location.
      react: r("node_modules/react"),
      "react-dom": r("node_modules/react-dom"),
      "react/jsx-runtime": r("node_modules/react/jsx-runtime"),
      "lucide-react": r("node_modules/lucide-react"),
      anser: r("node_modules/anser"),
      "@mcp-ui/client": r("node_modules/@mcp-ui/client"),
      "@testing-library/react": r("node_modules/@testing-library/react"),
      "@testing-library/jest-dom": r("node_modules/@testing-library/jest-dom"),
      "@testing-library/user-event": r("node_modules/@testing-library/user-event"),
    },
  },
  // Allow Vite's dev server to serve files from outside the web/ project root
  // so that co-located plugin tests in pkg/**/ui/ can be loaded.
  server: { fs: { allow: [".."] } },
  test: {
    environment: "jsdom",
    globals: false,
    css: false,
    include: ["packages/**/*.test.{ts,tsx}", "../pkg/**/ui/**/*.test.{ts,tsx}"],
    server: { deps: { inline: [/@ap\//] } },
  },
});
