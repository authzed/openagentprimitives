// Pagefind's bundle is written to public/_pagefind by `pnpm build`, after
// Next has compiled everything, so the bundler must not try to resolve it.
// Under `next dev` there is no bundle and the import rejects.
export interface PagefindResult {
  url: string;
  meta: { title?: string };
  excerpt: string;
}

export interface Pagefind {
  search(q: string): Promise<{ results: { data(): Promise<PagefindResult> }[] }>;
}

const BUNDLE = "/_pagefind/pagefind.js";

const importBundle = () => import(/* webpackIgnore: true */ /* turbopackIgnore: true */ BUNDLE) as Promise<Pagefind>;

export async function loadPagefind(importer: () => Promise<Pagefind> = importBundle): Promise<Pagefind | null> {
  try {
    return await importer();
  } catch {
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
