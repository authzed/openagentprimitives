// compile-page.ts is the command behind `mage ui:compile`: one page.tsx in,
// one page.view.json out (pretty, trailing newline, so the committed output
// diffs like source) plus a page.view.sha256 sidecar holding the hash of the
// source that produced it. The sidecar is what lets Go refuse a stale view
// without a Node toolchain: pkg/platform/builderbundle re-hashes the embedded
// page.tsx and compares. A refusal prints `file:line:col: text` and exits 1 —
// the same shape a compiler error takes, so an editor can jump to it.
//
// Runs under `node --experimental-strip-types` (Node 22): no bundler, no
// extra runner dependency. The explicit `.ts` extension on the import is what
// that mode requires; vitest resolves it the same way.
import { createHash } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import { CompileError, compilePage } from "../src/compile.ts";

const [, , inPath, outPath] = process.argv;
if (!inPath || !outPath) {
  console.error("usage: compile-page.ts <page.tsx> <page.view.json>");
  process.exit(2);
}
// Read the source once, as bytes: the same bytes are both compiled and
// hashed, so the sidecar can never describe a different read of the file.
const source = readFileSync(inPath);
const shaPath = outPath.replace(/\.json$/, "") + ".sha256";
try {
  const node = compilePage(source.toString("utf8"), inPath);
  writeFileSync(outPath, JSON.stringify(node, null, 2) + "\n");
  writeFileSync(shaPath, createHash("sha256").update(source).digest("hex") + "\n");
} catch (e) {
  if (e instanceof CompileError) {
    console.error(`${inPath}:${e.message}`);
    process.exit(1);
  }
  throw e;
}
