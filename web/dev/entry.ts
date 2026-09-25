// Dev-only universal entry: vite serves this with HMR. It reads #root[data-app]
// and dynamically imports the matching plugin entry (or the system app), pairing
// each pkg/**/ui/app.json key with its sibling index.tsx.
const apps = import.meta.glob("../../pkg/**/ui/**/index.tsx");
const metas = import.meta.glob("../../pkg/**/ui/**/app.json", { eager: true }) as Record<string, { default?: { key?: string }; key?: string }>;
const system = import.meta.glob("../packages/runtime/src/system/index.tsx");

const key = document.getElementById("root")?.dataset.app ?? "";

function loaderFor(appKey: string): (() => Promise<unknown>) | undefined {
  if (appKey === "system") return Object.values(system)[0];
  for (const [ajPath, meta] of Object.entries(metas)) {
    const k = meta.default?.key ?? meta.key;
    if (k === appKey) {
      const idx = ajPath.replace(/app\.json$/, "index.tsx");
      return apps[idx];
    }
  }
  return undefined;
}

const load = loaderFor(key);
if (load) void load();
else console.error(`[ap-dev] no entry for appKey "${key}"`);
