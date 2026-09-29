// Static docs validator. Runs before `next build` (via `pnpm check`).
// It does NOT judge whether a claim is true — that is the authoring agent's job.
// It enforces structure:
//   1. Every media reference in an MDX guide resolves in _manifest.json with the
//      right asset kind, and the referenced file exists under public/.
//   2. Every manifest entry's file exists on disk.
//   3. Every .refs.yaml is well-formed: each ref has id + claim + exactly one
//      anchor (symbol | file+lines | production), and the code anchor exists in
//      the repo.
import { readFileSync, existsSync, readdirSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { parse as parseYaml } from "yaml";
import { anchorIds, checkLinks } from "../lib/links";

const siteDir = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const repoRoot = path.dirname(siteDir);
const guidesDir = path.join(siteDir, "content/docs");
const publicDir = path.join(siteDir, "public");
const manifestPath = path.join(siteDir, "content/_manifest.json");

const errors: string[] = [];
const err = (m: string) => errors.push(m);

interface MediaAsset {
  kind?: string;
  webm?: string;
  mp4?: string;
  poster?: string;
  src?: string;
}
const manifest: Record<string, MediaAsset> = JSON.parse(
  readFileSync(manifestPath, "utf8"),
);

const publicRel = (p: string) => path.join(publicDir, p.replace(/^\//, ""));

// 1 & 2: manifest entries point at files that exist.
for (const [name, asset] of Object.entries(manifest)) {
  for (const f of [asset.webm, asset.mp4, asset.poster, asset.src]) {
    if (f && !existsSync(publicRel(f)))
      err(`manifest[${name}]: file not found: ${f}`);
  }
}

// Component → required asset kind.
const KIND_FOR: Record<string, string> = {
  Clip: "clip",
  Screenshot: "screenshot",
  ScreenshotSeries: "screenshot",
  Video: "video",
};

const mdxFiles = readdirSync(guidesDir).filter((f) => f.endsWith(".mdx"));
const refFiles = readdirSync(guidesDir).filter((f) => f.endsWith(".refs.yaml"));

// 1: MDX media references resolve.
const mediaRe =
  /<(Clip|Screenshot|ScreenshotSeries|Video)\b[^>]*\bname="([^"]+)"/g;
for (const file of mdxFiles) {
  const src = readFileSync(path.join(guidesDir, file), "utf8");
  let m: RegExpExecArray | null;
  while ((m = mediaRe.exec(src)) !== null) {
    const [, comp, name] = m;
    const wantKind = KIND_FOR[comp];
    if (comp === "ScreenshotSeries") {
      if (!manifest[`${name}-1`])
        err(
          `${file}: <ScreenshotSeries name="${name}"> has no ${name}-1 in manifest`,
        );
      continue;
    }
    const asset = manifest[name];
    if (!asset) err(`${file}: <${comp} name="${name}"> not in manifest`);
    else if (asset.kind && asset.kind !== wantKind)
      err(
        `${file}: <${comp} name="${name}"> expected kind ${wantKind}, manifest says ${asset.kind}`,
      );
  }
  // A refs sidecar is optional (overview pages are broad prose); when present it
  // is validated below. Detailed guides carry one for anti-hallucination checks.
}

// 3: refs are well-formed and their code anchors exist.
const anchorExists = (anchor: string): boolean => {
  const filePart = anchor.split("#")[0];
  return existsSync(path.join(repoRoot, filePart));
};
for (const file of refFiles) {
  let doc: { page?: string; refs?: unknown[] };
  try {
    doc = parseYaml(readFileSync(path.join(guidesDir, file), "utf8"));
  } catch (e) {
    err(`${file}: YAML parse error: ${(e as Error).message}`);
    continue;
  }
  if (!doc.page) err(`${file}: missing 'page'`);
  if (!Array.isArray(doc.refs) || doc.refs.length === 0) {
    err(`${file}: 'refs' must be a non-empty list`);
    continue;
  }
  for (const raw of doc.refs) {
    const ref = raw as Record<string, unknown>;
    const id = String(ref.id ?? "(no id)");
    if (!ref.id) err(`${file}: a ref is missing 'id'`);
    if (!ref.claim) err(`${file}: ref ${id} is missing 'claim'`);
    const anchors = ["symbol", "file", "production"].filter((k) => ref[k]);
    if (anchors.length !== 1)
      err(
        `${file}: ref ${id} must have exactly one of symbol|file|production (has ${anchors.length})`,
      );
    if (ref.symbol && !anchorExists(String(ref.symbol)))
      err(`${file}: ref ${id} symbol not found: ${ref.symbol}`);
    if (ref.file) {
      if (!existsSync(path.join(repoRoot, String(ref.file))))
        err(`${file}: ref ${id} file not found: ${ref.file}`);
      if (!/^\d+-\d+$/.test(String(ref.lines ?? "")))
        err(`${file}: ref ${id} needs 'lines: N-M' (got ${ref.lines})`);
    }
    if (ref.production && !manifest[String(ref.production)])
      err(`${file}: ref ${id} production not in manifest: ${ref.production}`);
  }
}

// 4: every /docs link names a guide, and every #anchor names an id in it. The
// landing page is plain TSX, so it is scanned the same way; a templated href
// (the OWASP table) cannot be, and is covered by app/(landing)/owasp.test.ts.
const guideSources = new Map(
  mdxFiles.map((f) => [
    f.slice(0, -4),
    readFileSync(path.join(guidesDir, f), "utf8"),
  ]),
);
const guideAnchors = new Map(
  [...guideSources].map(([slug, src]) => [slug, anchorIds(src)]),
);
const linkFiles = [
  ...[...guideSources].map(([slug, src]) => ({
    file: `content/docs/${slug}.mdx`,
    src,
  })),
  ...readdirSync(path.join(siteDir, "app"), {
    recursive: true,
    encoding: "utf8",
  })
    .filter((f) => f.endsWith(".tsx"))
    .map((f) => ({
      file: `app/${f}`,
      src: readFileSync(path.join(siteDir, "app", f), "utf8"),
    })),
];
for (const p of checkLinks(linkFiles, guideAnchors))
  err(`${p.file}: ${p.href}: ${p.reason}`);

if (errors.length) {
  console.error(`check FAILED with ${errors.length} problem(s):`);
  for (const e of errors) console.error(`  ✗ ${e}`);
  process.exit(1);
}
console.log(
  `check OK — ${mdxFiles.length} guide(s), ${refFiles.length} refs sidecar(s), ${Object.keys(manifest).length} media entr(ies).`,
);
