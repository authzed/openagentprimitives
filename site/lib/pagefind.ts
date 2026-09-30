// Pagefind's bundle is written to public/_pagefind by `pnpm build`, after
// Next has compiled everything, so the bundler must not try to resolve it.
// On a clean checkout `next dev` has no bundle and the import rejects; once
// any `pnpm build` has run, `public/_pagefind` exists on disk and `next dev`
// serves that (possibly stale) index like any other static file.
export interface PagefindResult {
  url: string;
  meta: { title?: string };
  excerpt: string;
}

export interface Pagefind {
  search(
    q: string,
  ): Promise<{ results: { data(): Promise<PagefindResult> }[] }>;
}

const BUNDLE = "/_pagefind/pagefind.js";

const importBundle = () =>
  import(
    /* webpackIgnore: true */ /* turbopackIgnore: true */ BUNDLE
  ) as Promise<Pagefind>;

// A missing bundle is expected under `next dev` and returns null so the UI can
// say so. It is still logged: on a deployed site the same failure means the
// index 404ed or was blocked, and the console is the only place that shows why.
export async function loadPagefind(
  importer: () => Promise<Pagefind> = importBundle,
  log: (message: string, err: unknown) => void = console.error,
): Promise<Pagefind | null> {
  try {
    return await importer();
  } catch (err) {
    log("docs search: could not load the search index", err);
    return null;
  }
}

// Pagefind records the prerendered file's path (`/docs/x.html`); the route is
// `/docs/x`.
export function toPath(url: string): string {
  const [p, hash] = url.split("#");
  const clean = p.replace(/\/index\.html$/, "/").replace(/\.html$/, "");
  return hash ? `${clean}#${hash}` : clean;
}
