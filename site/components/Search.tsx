"use client";
import Link from "next/link";
import { useRef, useState } from "react";
import {
  loadPagefind,
  toPath,
  type Pagefind,
  type PagefindResult,
} from "@/lib/pagefind";

type State =
  | { kind: "idle" }
  | { kind: "unavailable" }
  | { kind: "results"; items: PagefindResult[] };

export function Search() {
  const pf = useRef<Pagefind | null | undefined>(undefined);
  // Bumped at the start of each onInput call; a stale (slower) call's result
  // is dropped instead of overwriting a newer call's state.
  const gen = useRef(0);
  const [state, setState] = useState<State>({ kind: "idle" });

  async function ensure() {
    if (pf.current === undefined) pf.current = await loadPagefind();
    if (pf.current === null) setState({ kind: "unavailable" });
    return pf.current;
  }

  async function onInput(q: string) {
    const my = ++gen.current;
    const engine = await ensure();
    if (!engine) return;
    if (!q.trim()) {
      if (my === gen.current) setState({ kind: "idle" });
      return;
    }
    const { results } = await engine.search(q);
    const items = await Promise.all(results.slice(0, 8).map((r) => r.data()));
    if (my === gen.current) setState({ kind: "results", items });
  }

  return (
    <div className="doc-search" role="search">
      <input
        className="doc-search-input"
        type="search"
        placeholder="Search docs"
        aria-label="Search docs"
        onFocus={ensure}
        onChange={(e) => void onInput(e.target.value)}
      />
      {state.kind === "unavailable" && (
        <p className="doc-search-note">
          Search is available after <code>pnpm build</code>.
        </p>
      )}
      {state.kind === "results" && (
        <ul className="doc-search-results">
          {state.items.length === 0 && (
            <li className="doc-search-note">No matches.</li>
          )}
          {state.items.map((r) => (
            <li key={r.url}>
              <Link href={toPath(r.url)}>{r.meta.title ?? toPath(r.url)}</Link>
              {/* Pagefind's excerpt is its own escaped text with <mark> highlights. */}
              <p dangerouslySetInnerHTML={{ __html: r.excerpt }} />
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
