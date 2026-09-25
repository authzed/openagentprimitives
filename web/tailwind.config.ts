import type { Config } from "tailwindcss";
import path from "node:path";
import preset from "@ap/design/tailwind-preset";

const repoRoot = path.resolve(process.cwd(), "..");

export default {
  presets: [preset as Config],
  content: [
    // `packages/*/src` (single * for the package dir) so the glob never descends
    // into a package's nested node_modules — Tailwind warns on broad ** patterns.
    path.join(process.cwd(), "packages/*/src/**/*.{ts,tsx}"),
    path.join(repoRoot, "pkg/**/ui/**/*.{ts,tsx}"),
  ],
} satisfies Config;
